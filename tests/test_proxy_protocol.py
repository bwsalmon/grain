from grain.proxy.protocol import (
    GitRequest,
    err_pkt,
    is_valid_git_request,
    parse_path,
    pkt_line,
)


def test_parses_info_refs():
    assert parse_path("/owner/repo.git/info/refs") == GitRequest(
        "owner", "repo", "info/refs"
    )


def test_parses_without_dot_git_suffix():
    assert parse_path("/owner/repo/git-upload-pack") == GitRequest(
        "owner", "repo", "git-upload-pack"
    )


def test_parses_git_receive_pack():
    assert parse_path("/owner/repo.git/git-receive-pack") == GitRequest(
        "owner", "repo", "git-receive-pack"
    )


def test_rejects_anything_else():
    assert parse_path("/owner/repo.git/objects/info/packs") is None
    assert parse_path("/owner/repo.git/HEAD") is None
    assert parse_path("/") is None
    assert parse_path("/just-one-segment") is None


def test_valid_git_request_needs_a_git_user_agent():
    assert not is_valid_git_request("Mozilla/5.0", "*/*", "info/refs")
    assert is_valid_git_request("git/2.39.2", "*/*", "info/refs")


def test_valid_git_request_checks_accept_for_pack_endpoints():
    ua = "git/2.39.2"
    assert is_valid_git_request(ua, "application/x-git-upload-pack-result", "git-upload-pack")
    assert not is_valid_git_request(ua, "application/x-git-receive-pack-result", "git-upload-pack")
    assert not is_valid_git_request(ua, "*/*", "git-upload-pack")


def test_pkt_line_length_prefix():
    assert pkt_line(b"hello\n") == b"000ahello\n"


def test_err_pkt_is_a_valid_pkt_line():
    out = err_pkt("nope")
    length = int(out[:4], 16)
    assert length == len(out)
    assert out[4:] == b"ERR nope\n"
