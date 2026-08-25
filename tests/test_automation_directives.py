import pytest

from grain.automation.directives import (
    DirectiveError, RepoRef, parse_directives, strip_directives,
)


def test_a_repo_directive_names_the_target_repo():
    d = parse_directives(["please fix the thing\n\n/repo acme/widgets\n"])
    assert d.target == RepoRef("acme", "widgets")
    assert d.pr is None
    assert d.base is None


def test_a_directive_is_found_anywhere_in_the_body():
    """An issue template puts prose, headings and a checklist above whatever
    a human types -- a directive that only counted on line one would be
    unusable with one.
    """
    body = "## Summary\n\nsomething broke\n\n## Where\n\n/repo acme/widgets\n\nthanks"
    assert parse_directives([body]).target == RepoRef("acme", "widgets")


def test_pr_and_base_directives_are_read_too():
    d = parse_directives(["/repo acme/widgets\n/pr 42\n/base develop\n"])
    assert (d.target, d.pr, d.base) == (RepoRef("acme", "widgets"), 42, "develop")


def test_auto_merge_directive_is_read():
    d = parse_directives(["/repo acme/widgets\n/base develop\n/auto-merge true\n"])
    assert d.auto_merge is True


def test_auto_merge_defaults_to_false():
    assert parse_directives(["/repo acme/widgets"]).auto_merge is False


def test_auto_merge_is_sticky_across_texts():
    """No directive line can unset a flag once an earlier text set it --
    the same reasoning a label's stickiness has.
    """
    d = parse_directives(["/auto-merge true", "just a reply, no directives"])
    assert d.auto_merge is True


def test_a_pr_directive_tolerates_a_leading_hash():
    assert parse_directives(["/pr #42"]).pr == 42


def test_a_line_that_is_not_a_known_directive_is_left_alone():
    """A body full of shell commands and absolute paths must not be
    misread: only the three known names are directives.
    """
    body = "run this:\n/usr/bin/env python3\n/opt/grain/deploy.sh --now\n"
    d = parse_directives([body])
    assert d.target is None
    assert strip_directives(body) == body.strip()


def test_a_malformed_repo_is_an_error_naming_what_a_good_one_looks_like():
    with pytest.raises(DirectiveError) as exc:
        parse_directives(["/repo https://github.com/acme/widgets"])
    assert "owner/name" in str(exc.value)


def test_a_non_numeric_pr_is_an_error():
    with pytest.raises(DirectiveError):
        parse_directives(["/pr soon"])


def test_two_conflicting_directives_in_one_text_are_an_error():
    """Which repo the work lands in is exactly the thing not to guess at."""
    with pytest.raises(DirectiveError) as exc:
        parse_directives(["/repo acme/widgets\n/repo acme/other\n"])
    assert "ambiguous" in str(exc.value)


def test_the_same_directive_repeated_verbatim_is_fine():
    # A quoted body, a restated line -- no ambiguity to fail closed on.
    d = parse_directives(["/repo acme/widgets\nsome text\n/repo acme/widgets\n"])
    assert d.target == RepoRef("acme", "widgets")


def test_a_later_text_overrides_an_earlier_one():
    """The repair path: a maintainer replies with the corrected directive
    rather than editing the original body.
    """
    d = parse_directives(["/repo acme/wrong", "sorry, wrong one\n/repo acme/right"])
    assert d.target == RepoRef("acme", "right")


def test_a_later_text_leaves_directives_it_does_not_mention_alone():
    d = parse_directives(["/repo acme/widgets\n/base develop", "/pr 7"])
    assert (d.target, d.pr, d.base) == (RepoRef("acme", "widgets"), 7, "develop")


def test_strip_directives_removes_only_the_directive_lines():
    body = "fix the bug\n/repo acme/widgets\n\nit happens on startup\n"
    assert strip_directives(body) == "fix the bug\n\nit happens on startup"


def test_repo_ref_renders_as_owner_slash_name():
    assert str(RepoRef("acme", "widgets")) == "acme/widgets"
