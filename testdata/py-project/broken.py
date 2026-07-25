"""Deliberate type error, kept out of the reference graph on purpose."""


class Widget:
    def __init__(self) -> None:
        self.id = "w1"


widget = Widget()
BROKEN_VALUE = widget.missing_prop
