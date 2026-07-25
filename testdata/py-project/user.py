"""User model and lookup helpers."""


class User:
    def __init__(self, user_id: str, name: str) -> None:
        self.id = user_id
        self.name = name


def get_user_by_id(user_id: str) -> User:
    """Return a User for the given id."""
    return User(user_id, "user-" + user_id)
