import json

from grain.automation.transcript import parse_event, parse_transcript

# Shapes below are the real Claude Code event shape, confirmed directly
# against an actual transcript file (~/.claude/projects/.../*.jsonl) and
# against Claude Code's own headless-mode docs — see capture.py's docstring.


def test_parse_event_skips_blank_lines():
    assert parse_event("") is None
    assert parse_event("   \n") is None


def test_parse_event_handles_a_plain_string_user_message():
    line = json.dumps({
        "type": "user",
        "message": {"role": "user", "content": "please fix the bug in auth.py"},
    })
    event = parse_event(line)
    assert event.role == "user"
    assert "please fix the bug in auth.py" in event.summary
    assert event.detail == "please fix the bug in auth.py"


def test_parse_event_handles_assistant_text_block():
    line = json.dumps({
        "type": "assistant",
        "message": {
            "role": "assistant",
            "content": [{"type": "text", "text": "I'll look at the file now."}],
        },
    })
    event = parse_event(line)
    assert event.role == "assistant"
    assert "I'll look at the file now." in event.detail


def test_parse_event_handles_tool_use_block():
    line = json.dumps({
        "type": "assistant",
        "message": {
            "role": "assistant",
            "content": [{
                "type": "tool_use", "id": "toolu_1", "name": "Bash",
                "input": {"command": "ls -la"},
            }],
        },
    })
    event = parse_event(line)
    assert "tool_use: Bash" in event.detail
    assert "ls -la" in event.detail


def test_parse_event_handles_tool_result_block():
    line = json.dumps({
        "type": "user",
        "message": {
            "role": "user",
            "content": [{
                "type": "tool_result", "tool_use_id": "toolu_1",
                "content": "file1.py\nfile2.py", "is_error": False,
            }],
        },
    })
    event = parse_event(line)
    assert "tool_result (ok)" in event.detail
    assert "file1.py" in event.detail


def test_parse_event_handles_a_tool_result_whose_content_is_itself_a_list_of_blocks():
    # A real transcript shape `content: "..."` above doesn't cover: some
    # tool results carry their own list of typed content blocks rather
    # than a plain string.
    line = json.dumps({
        "type": "user",
        "message": {
            "role": "user",
            "content": [{
                "type": "tool_result", "tool_use_id": "toolu_1",
                "content": [{"type": "text", "text": "file1.py"},
                            {"type": "text", "text": "file2.py"}],
                "is_error": False,
            }],
        },
    })
    event = parse_event(line)
    assert "tool_result (ok)" in event.detail
    assert "file1.py" in event.detail and "file2.py" in event.detail


def test_parse_event_content_that_is_neither_a_string_nor_a_list_renders_as_empty():
    # An unrecognized/future message shape must not crash the viewer.
    line = json.dumps({"type": "user", "message": {"role": "user", "content": 42}})
    event = parse_event(line)
    assert event.detail == ""


def test_parse_event_skips_a_non_dict_entry_in_a_content_list():
    line = json.dumps({
        "type": "assistant",
        "message": {
            "role": "assistant",
            "content": ["not a block", {"type": "text", "text": "still works"}],
        },
    })
    event = parse_event(line)
    assert event.detail == "still works"


def test_parse_event_marks_an_error_tool_result():
    line = json.dumps({
        "type": "user",
        "message": {
            "role": "user",
            "content": [{
                "type": "tool_result", "tool_use_id": "toolu_1",
                "content": "command not found", "is_error": True,
            }],
        },
    })
    event = parse_event(line)
    assert "tool_result (error)" in event.detail


def test_parse_event_skips_thinking_blocks_in_detail():
    line = json.dumps({
        "type": "assistant",
        "message": {
            "role": "assistant",
            "content": [
                {"type": "thinking", "thinking": "internal reasoning here"},
                {"type": "text", "text": "the visible answer"},
            ],
        },
    })
    event = parse_event(line)
    assert "internal reasoning here" not in event.detail
    assert "the visible answer" in event.detail


def test_parse_event_handles_the_final_result_line():
    line = json.dumps({
        "type": "result", "result": "Fixed the bug.",
        "total_cost_usd": 0.42, "num_turns": 5, "is_error": False,
        "session_id": "abc-123",
    })
    event = parse_event(line)
    assert event.role == "result"
    assert "Fixed the bug." in event.summary
    assert "0.42" in event.summary


def test_parse_event_handles_a_system_init_line():
    line = json.dumps({"type": "system", "subtype": "init", "model": "claude-sonnet-5"})
    event = parse_event(line)
    assert event.role == "system"
    assert "init" in event.summary


def test_parse_event_falls_back_gracefully_on_an_unknown_type():
    line = json.dumps({"type": "future_event_type", "some_field": "value"})
    event = parse_event(line)
    assert event.role == "future_event_type"
    assert "value" in event.detail


def test_parse_event_handles_malformed_json_without_raising():
    event = parse_event("{not valid json")
    assert event.role == "raw"
    assert "not valid json" in event.detail


def test_parse_transcript_parses_every_line_and_skips_blanks():
    text = "\n".join([
        json.dumps({"type": "system", "subtype": "init"}),
        "",
        json.dumps({"type": "user", "message": {"role": "user", "content": "hi"}}),
        json.dumps({"type": "result", "result": "done"}),
    ])
    events = parse_transcript(text)
    assert len(events) == 3
    assert [e.role for e in events] == ["system", "user", "result"]


def test_parse_transcript_of_empty_text_is_an_empty_list():
    assert parse_transcript("") == []
