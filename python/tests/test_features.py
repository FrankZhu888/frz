import base64
import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

frz = SourceFileLoader("frz_under_test", str(Path(__file__).resolve().parents[1] / "frz")).load_module()

PNG_1X1 = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
)


class AttachmentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.old_attach_dir = frz.ATTACH_DIR
        self.old_sess_dir = frz.SESS_DIR
        frz.ATTACH_DIR = Path(self.temp.name) / "attachments"
        frz.SESS_DIR = Path(self.temp.name) / "sessions"

    def tearDown(self):
        frz.ATTACH_DIR = self.old_attach_dir
        frz.SESS_DIR = self.old_sess_dir
        self.temp.cleanup()

    def test_literal_at_and_email(self):
        text, refs = frz._parse_at_references("mail a@example.com @@literal")
        self.assertEqual(refs, [])
        self.assertEqual(text, "mail a@example.com @literal")

    def test_text_image_and_quoted_path(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            text_file = root / "a.txt"
            text_file.write_text("hello 中文", encoding="utf-8")
            image_file = root / "image with space.png"
            image_file.write_bytes(PNG_1X1)
            message = frz._build_user_message(
                f"分析 @{text_file} @'{image_file}'", "openai_responses"
            )
            self.assertEqual(message["content"], "分析")
            self.assertEqual([a["kind"] for a in message["attachments"]], ["text", "image"])
            for attachment in message["attachments"]:
                self.assertNotIn("data", attachment)
                self.assertNotIn("content", attachment)
                self.assertTrue((frz.ATTACH_DIR / attachment["sha256"]).is_file())
            parts = frz._responses_content(message)
            self.assertEqual(parts[0]["type"], "input_text")
            self.assertIn("hello 中文", parts[0]["text"])
            self.assertIn("a.txt", parts[0]["text"])
            self.assertEqual(parts[1]["type"], "input_image")
            self.assertTrue(parts[1]["image_url"].startswith("data:image/png;base64,"))
            json.dumps(message, ensure_ascii=False)  # 会话可序列化

    def test_only_attachment_gets_default_prompt(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / "a.txt"
            path.write_text("data", encoding="utf-8")
            message = frz._build_user_message(f"@{path}", "openai_responses")
            self.assertEqual(message["content"], "Please analyze the attachment(s).")

    def test_old_message_is_unchanged(self):
        parts = frz._responses_content({"role": "user", "content": "plain"})
        self.assertEqual(parts, [{"type": "input_text", "text": "plain"}])

    def test_other_provider_and_missing_file_fail_before_history(self):
        with self.assertRaises(ValueError):
            frz._build_user_message("@anything", "anthropic")
        session = {"messages": []}
        with contextlib.redirect_stdout(io.StringIO()):
            frz.send_message(session, "@definitely-not-a-real-file", "openai_responses", "unused")
        self.assertEqual(session["messages"], [])

    def test_request_body_contains_image_without_network(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / "pixel.png"
            path.write_bytes(PNG_1X1)
            message = frz._build_user_message(f"看图 @{path}", "openai_responses")

            class FakeResponse:
                def __enter__(self):
                    return self

                def __exit__(self, *args):
                    return False

                def __iter__(self):
                    return iter([
                        b"event: response.completed\n",
                        b'data: {"type":"response.completed"}\n',
                    ])

            with mock.patch.object(frz.urllib.request, "urlopen", return_value=FakeResponse()) as opened:
                frz.call_openai_responses(
                    [message], "", "vision-test", "secret", base_url="https://example.invalid/api/v3"
                )
            request = opened.call_args.args[0]
            payload = json.loads(request.data.decode("utf-8"))
            content = payload["input"][0]["content"]
            self.assertEqual(content[0]["type"], "input_text")
            self.assertEqual(content[1]["type"], "input_image")
            self.assertEqual(payload["model"], "vision-test")

    def test_identical_attachments_share_one_blob(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / "same.txt"
            path.write_text("same content", encoding="utf-8")
            message = frz._build_user_message(f"@{path} @{path}", "openai_responses")
            hashes = [a["sha256"] for a in message["attachments"]]
            self.assertEqual(hashes[0], hashes[1])
            self.assertEqual(len(list(frz.ATTACH_DIR.iterdir())), 1)

    def test_legacy_inline_attachments_migrate_on_save(self):
        session = frz.new_session("legacy", "openai_responses", "test", "", "https://example.invalid")
        image_data = base64.b64encode(PNG_1X1).decode("ascii")
        session["messages"].append({
            "role": "user",
            "content": "legacy",
            "attachments": [
                {"kind": "text", "name": "old.txt", "content": "old text", "size": 8},
                {"kind": "image", "name": "old.png", "media_type": "image/png", "data": image_data, "size": len(PNG_1X1)},
            ],
        })
        frz.save_session(session)
        stored = json.loads(frz.session_path("legacy").read_text(encoding="utf-8"))
        attachments = stored["messages"][0]["attachments"]
        for attachment in attachments:
            self.assertNotIn("data", attachment)
            self.assertNotIn("content", attachment)
            self.assertTrue((frz.ATTACH_DIR / attachment["sha256"]).is_file())
        parts = frz._responses_content(stored["messages"][0])
        self.assertIn("old text", parts[0]["text"])
        self.assertEqual(parts[1]["type"], "input_image")

    def test_missing_or_corrupt_blob_fails_closed(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / "a.txt"
            path.write_text("data", encoding="utf-8")
            message = frz._build_user_message(f"@{path}", "openai_responses")
            blob = frz.ATTACH_DIR / message["attachments"][0]["sha256"]
            blob.unlink()
            with self.assertRaises(RuntimeError):
                frz._responses_content(message)
            blob.write_bytes(b"replacement")
            with self.assertRaises(RuntimeError):
                frz._responses_content(message)

    def test_clean_preserves_references_and_recent_orphans(self):
        with tempfile.TemporaryDirectory() as td:
            path = Path(td) / "keep.txt"
            path.write_text("keep", encoding="utf-8")
            message = frz._build_user_message(f"@{path}", "openai_responses")
            session = frz.new_session("keep", "openai_responses", "test", "", "https://example.invalid")
            session["messages"].append(message)
            frz.save_session(session)
            referenced = frz.ATTACH_DIR / message["attachments"][0]["sha256"]

            orphan_digest = frz._store_attachment(b"orphan")
            orphan = frz.ATTACH_DIR / orphan_digest
            _, count, skipped = frz._clean_unreferenced_attachments(max_age=3600)
            self.assertEqual((count, skipped), (0, 1))
            self.assertTrue(orphan.is_file())
            frz._clean_unreferenced_attachments(max_age=0)
            self.assertFalse(orphan.exists())
            self.assertTrue(referenced.is_file())

    def test_clean_aborts_when_a_session_is_corrupt(self):
        frz.ensure_dirs()
        (frz.SESS_DIR / "corrupt.json").write_text("not json", encoding="utf-8")
        orphan = frz.ATTACH_DIR / frz._store_attachment(b"orphan")
        with self.assertRaises(RuntimeError):
            frz._clean_unreferenced_attachments(max_age=0)
        self.assertTrue(orphan.is_file())


class WebSearchTests(unittest.TestCase):
    def test_session_and_cli_defaults(self):
        session = frz.new_session("web", "openai_responses", "test", "", "https://example.invalid")
        self.assertTrue(session["web_search"])
        other = frz.new_session("other", "anthropic", "test", "")
        self.assertFalse(other["web_search"])
        enabled = frz.new_session("web", "openai_responses", "test", "", "https://example.invalid", web_search=True)
        self.assertTrue(enabled["web_search"])
        self.assertIsNone(frz.build_arg_parser().parse_args([]).web_search)
        self.assertTrue(frz.build_arg_parser().parse_args(["--web"]).web_search)
        self.assertFalse(frz.build_arg_parser().parse_args(["--no-web"]).web_search)

    def test_tools_and_search_events(self):
        class FakeResponse:
            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def __iter__(self):
                events = [
                    "response.web_search_call.in_progress",
                    "response.web_search_call.searching",
                    "response.web_search_call.completed",
                ]
                lines = []
                for event in events:
                    lines.extend([f"event: {event}\n".encode(), f'data: {{"type":"{event}"}}\n'.encode()])
                lines.extend([
                    b"event: response.output_text.delta\n",
                    b'data: {"type":"response.output_text.delta","delta":"done"}\n',
                    b"event: response.completed\n",
                    b'data: {"type":"response.completed"}\n',
                ])
                return iter(lines)

        states = []
        with mock.patch.object(frz.urllib.request, "urlopen", return_value=FakeResponse()) as opened:
            reply = frz.call_openai_responses(
                [{"role": "user", "content": "search"}], "", "test", "secret",
                base_url="https://example.invalid/api/v3", web_search=True, on_search=states.append,
            )
        self.assertEqual(reply, "done")
        self.assertEqual(states, ["in_progress", "searching", "completed"])
        payload = json.loads(opened.call_args.args[0].data.decode("utf-8"))
        self.assertEqual(payload["tools"], [{"type": "web_search"}])
        self.assertNotIn("tool_choice", payload)

    def test_web_search_disabled_omits_tools(self):
        class FakeResponse:
            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def __iter__(self):
                return iter([b"event: response.completed\n", b'data: {"type":"response.completed"}\n'])

        with mock.patch.object(frz.urllib.request, "urlopen", return_value=FakeResponse()) as opened:
            frz.call_openai_responses(
                [{"role": "user", "content": "plain"}], "", "test", "secret",
                base_url="https://example.invalid/api/v3",
            )
        payload = json.loads(opened.call_args.args[0].data.decode("utf-8"))
        self.assertNotIn("tools", payload)


class IndicatorTests(unittest.TestCase):
    def test_search_status_can_override_thinking_state(self):
        indicator = frz.ThinkingIndicator()
        indicator.set_state("Pondering")
        indicator.set_state("Searching")
        self.assertEqual(indicator._state, "Pondering")
        indicator.set_state("Searching", force=True)
        self.assertEqual(indicator._state, "Searching")

    def test_completed_search_returns_to_thinking_status(self):
        session = frz.new_session("status", "openai_responses", "test", "", "https://example.invalid")

        def fake_call_model(*args, **kwargs):
            kwargs["on_search"]("searching")
            kwargs["on_search"]("completed")
            return "ok"

        with mock.patch.object(frz, "call_model", side_effect=fake_call_model), \
                mock.patch.object(frz, "save_session"), \
                mock.patch.object(frz.random, "choice", return_value="Pondering"), \
                mock.patch.object(frz.ThinkingIndicator, "set_state", autospec=True) as set_state:
            with contextlib.redirect_stdout(io.StringIO()):
                frz.send_message(session, "test", "openai_responses", "secret")
        states = [call.args[-1] for call in set_state.call_args_list]
        self.assertEqual(states, ["Searching", "Pondering"])
        self.assertTrue(all(call.kwargs["force"] for call in set_state.call_args_list))

    def test_first_delta_does_not_stop_spinner_before_visible_output(self):
        events = []
        stream = frz.StreamRenderer(on_output=lambda: events.append("visible"))
        with contextlib.redirect_stdout(io.StringIO()):
            stream.feed("partial")
            self.assertEqual(events, [])
            stream.feed(" line\nnext")
            self.assertEqual(events, ["visible"])
            stream.finish()
        self.assertEqual(events, ["visible"])

    def test_finish_without_newline_still_signals_visible_output(self):
        events = []
        stream = frz.StreamRenderer(on_output=lambda: events.append("visible"))
        with contextlib.redirect_stdout(io.StringIO()):
            stream.feed("single line without newline")
            self.assertEqual(events, [])
            stream.finish()
        self.assertEqual(events, ["visible"])


class ShellAndCompletionTests(unittest.TestCase):
    def test_shell_classification_and_pwd(self):
        self.assertFalse(frz._handle_shell_input("echo 你好"))
        completed = frz.subprocess.CompletedProcess(["pwd"], 0)
        with mock.patch.object(frz.subprocess, "run", return_value=completed) as run:
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertTrue(frz._handle_shell_input("pwd"))
        run.assert_called_once_with("pwd", shell=True, executable="/bin/sh")

    def test_cd_persists(self):
        old = os.getcwd()
        with tempfile.TemporaryDirectory() as td:
            try:
                with contextlib.redirect_stdout(io.StringIO()):
                    self.assertTrue(frz._handle_shell_input(f"cd {td}"))
                self.assertEqual(os.path.realpath(os.getcwd()), os.path.realpath(td))
            finally:
                os.chdir(old)

    def test_path_completion(self):
        old = os.getcwd()
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            (root / "a.txt").write_text("a", encoding="utf-8")
            (root / "with space.png").write_bytes(PNG_1X1)
            os.chdir(root)
            try:
                self.assertIn("@a.txt", frz._completion_matches("@a", "@a"))
                self.assertIn("@'with space.png'", frz._completion_matches("@w", "@w"))
            finally:
                os.chdir(old)


if __name__ == "__main__":
    unittest.main(verbosity=2)
