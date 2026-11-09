import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

f = SourceFileLoader("frz_fix_target", str(Path(__file__).resolve().parents[1] / "frz")).load_module()


class FakeResponse:
    def __init__(self, lines):
        self.lines = lines

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False

    def __iter__(self):
        return iter(self.lines)


class FixTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        root = Path(self.temp.name)
        self.old_dirs = (f.SESS_DIR, f.ATTACH_DIR, f.CONFIG_FILE)
        f.SESS_DIR = root / "sessions"
        f.ATTACH_DIR = root / "attachments"
        f.CONFIG_FILE = root / "config.json"
        f.ensure_dirs()

    def tearDown(self):
        f.SESS_DIR, f.ATTACH_DIR, f.CONFIG_FILE = self.old_dirs
        self.temp.cleanup()

    def make_session(self, name, text=None, provider="openai_responses"):
        session = f.new_session(name, provider, "test", "", "https://example.invalid", web_search=False)
        if text is not None:
            session["messages"].append({"role": "user", "content": text})
        return session

    def run_repl(self, inputs, session):
        with mock.patch("builtins.input", side_effect=inputs + ["/exit"]):
            with contextlib.redirect_stdout(io.StringIO()) as output:
                f.repl(session, "test-key")
        return output.getvalue()

    def test_cross_provider_key_is_cleared_but_same_provider_cli_key_survives(self):
        with mock.patch.object(f, "resolve_api_key", return_value=None):
            self.assertIsNone(f._resumed_api_key("anthropic", "openai_responses", "old-key"))
            self.assertEqual(f._resumed_api_key("openai_responses", "openai_responses", "cli-key"), "cli-key")
        with mock.patch.object(f, "resolve_api_key", return_value="new-key"):
            self.assertEqual(f._resumed_api_key("anthropic", "openai_responses", "old-key"), "new-key")

    def test_missing_key_blocks_request_and_history(self):
        session = self.make_session("no-key")
        with mock.patch.object(f, "call_model") as call:
            with contextlib.redirect_stdout(io.StringIO()):
                f.send_message(session, "must not send", "openai_responses", None)
        call.assert_not_called()
        self.assertEqual(session["messages"], [])

    def test_startup_and_new_reject_existing_name(self):
        existing = self.make_session("work", "original")
        f.save_session(existing)
        argv = ["frz", "--provider", "openai_responses", "--api-key", "x", "--session-name", "work"]
        with mock.patch.object(f.sys, "argv", argv), mock.patch.object(f, "repl") as repl:
            with contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaises(SystemExit):
                    f.main()
        repl.assert_not_called()
        current = self.make_session("current")
        output = self.run_repl(["/new work"], current)
        self.assertIn("already exists", output)
        self.assertEqual(f.load_session("work")["messages"][0]["content"], "original")

    def test_clear_is_persistent_after_exit(self):
        session = self.make_session("clear", "old history")
        f.save_session(session)
        self.run_repl(["/clear"], session)
        self.assertEqual(f.load_session("clear")["messages"], [])

    def test_empty_session_can_rename_without_existing_file(self):
        session = self.make_session("empty-old")
        self.run_repl(["/rename empty-new"], session)
        self.assertEqual(session["name"], "empty-new")
        self.assertFalse(f.session_path("empty-old").exists())

    def test_atomic_failure_preserves_original_and_permissions_are_secure(self):
        session = self.make_session("atomic", "original")
        path = f.save_session(session)
        original = path.read_bytes()
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(f.SESS_DIR.stat().st_mode & 0o777, 0o700)

        replacement = self.make_session("atomic", "replacement")
        with mock.patch.object(f.os, "replace", side_effect=OSError("injected")):
            with self.assertRaises(OSError):
                f.save_session(replacement)
        self.assertEqual(path.read_bytes(), original)
        self.assertEqual(list(f.SESS_DIR.glob(".*.json.*")), [])

        os.chmod(path, 0o644)
        f.ensure_dirs()
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def call_stream(self, lines):
        with mock.patch.object(f.urllib.request, "urlopen", return_value=FakeResponse(lines)):
            return f.call_openai_responses(
                [{"role": "user", "content": "test"}], "", "test", "secret",
                base_url="https://example.invalid",
            )

    def test_eof_without_completed_is_an_error(self):
        with self.assertRaisesRegex(RuntimeError, "response.completed"):
            self.call_stream([
                b"event: response.output_text.delta\n",
                b'data: {"type":"response.output_text.delta","delta":"partial"}\n',
            ])

    def test_incomplete_event_is_an_error(self):
        with self.assertRaisesRegex(RuntimeError, "incomplete"):
            self.call_stream([
                b"event: response.incomplete\n",
                b'data: {"type":"response.incomplete","response":{"status":"incomplete","status_details":{"reason":"max_output_tokens"}}}\n',
            ])

    def test_data_only_failure_is_not_hidden_by_stale_event(self):
        with self.assertRaisesRegex(RuntimeError, "upstream failed"):
            self.call_stream([
                b"event: response.output_text.delta\n",
                b'data: {"type":"response.output_text.delta","delta":"partial"}\n',
                b'data: {"type":"response.failed","response":{"error":{"message":"upstream failed"}}}\n',
            ])

    def test_read_timeouts_are_wrapped_for_repl(self):
        with mock.patch.object(f.urllib.request, "urlopen", side_effect=TimeoutError("timed out")):
            with self.assertRaises(RuntimeError):
                f.call_openai_responses(
                    [{"role": "user", "content": "test"}], "", "test", "secret",
                    base_url="https://example.invalid",
                )
            with self.assertRaises(RuntimeError):
                f._http_post_json("https://example.invalid", {}, {})

    def test_blob_reuse_refreshes_mtime(self):
        blob = f.ATTACH_DIR / f._store_attachment(b"reused")
        os.utime(blob, (1_000_000_000, 1_000_000_000))
        f._store_attachment(b"reused")
        self.assertGreater(blob.stat().st_mtime, 1_000_000_000)


if __name__ == "__main__":
    unittest.main(verbosity=2)
