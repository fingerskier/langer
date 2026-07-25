"""UTF-16 offset fixture.

Line 9 (0-based) puts eight non-BMP characters before two symbols, chosen so
that a byte-offset or codepoint-offset misreading of the column lands outside
the intended identifier. See testdata/README.md.
"""

from user import get_user_by_id

ROCKETS = "🚀🚀🚀🚀🚀🚀🚀🚀"; rocket_name = get_user_by_id("42").name
