"""Consumers that live in a different module from the definition."""

from user import User, get_user_by_id

# get_user_by_id is mentioned in this comment but is not a reference.
NOTE = "get_user_by_id appears in this string literal too"


def describe_user(user_id: str) -> str:
    user: User = get_user_by_id(user_id)
    return NOTE + ": " + user.name


def greet_user(user_id: str) -> str:
    other = get_user_by_id(user_id)
    return "Hello, " + other.name
