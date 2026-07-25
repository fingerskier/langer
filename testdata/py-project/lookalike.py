"""A different function that merely shares the name.

A textual search for "get_user_by_id" finds it; a semantic reference search
rooted at user.py must NOT report it.
"""


def get_user_by_id(user_id: str) -> str:
    return "lookalike-" + user_id
