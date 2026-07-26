# `testdata/` — langer integration fixtures

Two minimal-but-real projects that the integration tests drive **real language
servers** against (SPEC §11.1), plus the security tripwire for the M6 test
(SPEC §9).

Everything in this file was **measured against the real servers**, not guessed.
See [Provenance](#provenance) for the exact versions and re-verification
procedure. If a documented value ever disagrees with a real server, re-verify
and fix this file — the tests assert against these numbers.

## Conventions (SPEC §4.3)

- **Positions are 0-based.** Line 0 is the first line of the file.
- **Characters are UTF-16 code units**, per the LSP default. This matters on the
  `unicode` fixtures — see [UTF-16 column handling](#utf-16-column-handling-ts)
  and its Python twin.
- Ranges are written `[line,char]-[line,char]`, end-exclusive.
- Paths are workspace-relative (relative to `ts-project/` or `py-project/`).
- `severity` values are raw LSP: `1 = Error`, `2 = Warning`, `3 = Information`,
  `4 = Hint`.
- `kind` values are raw LSP `SymbolKind`: `5 = Class`, `6 = Method`,
  `7 = Property`, `11 = Interface`, `12 = Function`, `13 = Variable`,
  `14 = Constant`.

---

# 1. `ts-project/` — TypeScript fixture

```
ts-project/
├── package.json           # no dependencies; `private: true`
├── tsconfig.json          # strict, noEmit, include: ["src"]
├── src/
│   ├── user.ts            # THE definition site
│   ├── service.ts         # 2 cross-file references + comment/string traps
│   ├── lookalike.ts       # same NAME, different symbol (semantic trap)
│   ├── unicode.ts         # 1 cross-file reference behind 8 non-BMP chars
│   └── broken.ts          # the one deliberate type error
└── node_modules/          # NOT a real install — the M6 tripwire lives here
```

> `node_modules/` contains **no** TypeScript and no real packages. It exists
> solely to hold the security tripwire. See [§3](#3-security-tripwire-m6).

## 1.1 Sources with 0-based line numbers

### `src/user.ts`

```
  0| export interface User {
  1|   id: string;
  2|   name: string;
  3| }
  4|
  5| export function getUserById(id: string): User {
  6|   return { id, name: "user-" + id };
  7| }
```

### `src/service.ts`

```
  0| import { getUserById, User } from "./user";
  1|
  2| // getUserById is mentioned in this comment but is not a reference.
  3| const NOTE = "getUserById appears in this string literal too";
  4|
  5| export function describeUser(id: string): string {
  6|   const user: User = getUserById(id);
  7|   return NOTE + ": " + user.name;
  8| }
  9|
 10| export function greetUser(id: string): string {
 11|   const other = getUserById(id);
 12|   return "Hello, " + other.name;
 13| }
```

### `src/lookalike.ts`

```
  0| // A DIFFERENT function that merely shares the name. A textual search for
  1| // "getUserById" finds it; a semantic reference search rooted at
  2| // src/user.ts must NOT report it.
  3| export function getUserById(id: string): string {
  4|   return "lookalike-" + id;
  5| }
```

### `src/unicode.ts`

```
  0| import { getUserById } from "./user";
  1|
  2| // Line 5 (0-based) puts eight non-BMP characters before two symbols, chosen so
  3| // that a byte-offset or codepoint-offset misreading of the column lands outside
  4| // the intended identifier. See testdata/README.md.
  5| export const ROCKETS = "🚀🚀🚀🚀🚀🚀🚀🚀"; export const rocketName = getUserById("42").name;
```

### `src/broken.ts`

```
  0| interface Widget {
  1|   id: string;
  2| }
  3|
  4| const widget: Widget = { id: "w1" };
  5|
  6| // Deliberate type error: TS2339. Kept out of the reference graph on purpose.
  7| export const brokenValue = widget.missingProp;
```

## 1.2 Symbol table (declarations)

`selectionRange` is the identifier; `range` is the whole declaration. Both come
verbatim from `textDocument/documentSymbol`.

| Symbol | Path | Kind | `selectionRange` (identifier) | `range` (full) |
|---|---|---|---|---|
| `User` | `src/user.ts` | 11 Interface | `[0,17]-[0,21]` | `[0,0]-[3,1]` |
| `User.id` | `src/user.ts` | 7 Property | `[1,2]-[1,4]` | `[1,2]-[1,13]` |
| `User.name` | `src/user.ts` | 7 Property | `[2,2]-[2,6]` | `[2,2]-[2,15]` |
| `getUserById` | `src/user.ts` | 12 Function | `[5,16]-[5,27]` | `[5,0]-[7,1]` |
| `describeUser` | `src/service.ts` | 12 Function | `[5,16]-[5,28]` | `[5,0]-[8,1]` |
| `greetUser` | `src/service.ts` | 12 Function | `[10,16]-[10,25]` | `[10,0]-[13,1]` |
| `NOTE` | `src/service.ts` | 14 Constant | `[3,6]-[3,10]` | `[3,6]-[3,61]` |
| `getUserById` (lookalike) | `src/lookalike.ts` | 12 Function | `[3,16]-[3,27]` | `[3,0]-[5,1]` |
| `ROCKETS` | `src/unicode.ts` | 14 Constant | `[5,13]-[5,20]` | `[5,13]-[5,41]` |
| `rocketName` | `src/unicode.ts` | 14 Constant | `[5,56]-[5,66]` | `[5,56]-[5,91]` |
| `Widget` | `src/broken.ts` | 11 Interface | `[0,10]-[0,16]` | `[0,0]-[2,1]` |
| `widget` | `src/broken.ts` | 14 Constant | `[4,6]-[4,12]` | `[4,6]-[4,35]` |
| `brokenValue` | `src/broken.ts` | 14 Constant | `[7,13]-[7,24]` | `[7,13]-[7,45]` |

`documentSymbol` on `src/user.ts` returns exactly two top-level symbols, in this
order: `getUserById` (12), then `User` (11). `getUserById` has two children of
kind 7 named `id` and `name` — these are the **object-literal properties in the
return statement**, not the interface's:

| Child | `selectionRange` | `range` |
|---|---|---|
| `id` | `[6,11]-[6,13]` | `[6,11]-[6,13]` |
| `name` | `[6,15]-[6,19]` | `[6,15]-[6,33]` |

The table above lists **declarations of interest**, not every node
`documentSymbol` emits. tsserver also returns local `const`s as children, so
`document_symbols` tests must not assert an exact top-level-plus-children count
without accounting for these measured extras:

| Parent | Child | Kind | `selectionRange` | `range` |
|---|---|---|---|---|
| `describeUser` (`src/service.ts`) | `user` | 14 Constant | `[6,8]-[6,12]` | `[6,8]-[6,36]` |
| `greetUser` (`src/service.ts`) | `other` | 14 Constant | `[11,8]-[11,13]` | `[11,8]-[11,31]` |
| `widget` (`src/broken.ts`) | `id` | 7 Property | `[4,25]-[4,27]` | `[4,25]-[4,33]` |
| `Widget` (`src/broken.ts`) | `id` | 7 Property | `[1,2]-[1,4]` | `[1,2]-[1,13]` |

Top-level order is: `src/service.ts` → `describeUser`, `greetUser`, `NOTE`;
`src/unicode.ts` → `rocketName`, `ROCKETS`; `src/broken.ts` → `brokenValue`,
`widget`, `Widget`; `src/lookalike.ts` → `getUserById` only.

## 1.3 `getUserById` — the reference graph

**Definition:** `src/user.ts` `[5,16]-[5,27]`.

`textDocument/references` at `src/user.ts` `[5,16]` returns exactly these, in
this order:

| # | Path | Range | `includeDeclaration: false` too? | What it is |
|---|---|---|---|---|
| 1 | `src/user.ts` | `[5,16]-[5,27]` | no (declaration) | the definition |
| 2 | `src/service.ts` | `[0,9]-[0,20]` | yes | import specifier |
| 3 | `src/service.ts` | `[6,21]-[6,32]` | yes | **cross-file call #1** |
| 4 | `src/service.ts` | `[11,16]-[11,27]` | yes | **cross-file call #2** |
| 5 | `src/unicode.ts` | `[0,9]-[0,20]` | yes | import specifier |
| 6 | `src/unicode.ts` | `[5,69]-[5,80]` | yes | **cross-file call #3** (behind emoji) |

So: 6 results with `includeDeclaration: true`, 5 with `false`, and **3 real
cross-file call sites in 2 different files**.

**`User` references** at `src/user.ts` `[0,17]`, `includeDeclaration: true` — 4
results in this order: `src/user.ts` `[0,17]-[0,21]`, `src/user.ts`
`[5,41]-[5,45]`, `src/service.ts` `[0,22]-[0,26]`, `src/service.ts`
`[6,14]-[6,18]`.

## 1.4 Textual-vs-semantic trap (TS)

The string `getUserById` occurs **10 times** across `src/`. Only 6 of them are
references to `src/user.ts`'s `getUserById`. A grep-based implementation returns
all 10 and fails these assertions:

| Path | Position | Reference? | Why |
|---|---|---|---|
| `src/service.ts` | `[2,3]-[2,14]` | ❌ | inside a `//` comment |
| `src/service.ts` | `[3,14]-[3,25]` | ❌ | inside a string literal |
| `src/lookalike.ts` | `[1,4]-[1,15]` | ❌ | inside a `//` comment |
| `src/lookalike.ts` | `[3,16]-[3,27]` | ❌ | a **different symbol** with the same name |
| `src/lookalike.ts` | `[3,16]` refs | — | returns **only itself**, 1 result |

Verified negative probes:

- `textDocument/definition` at `src/service.ts` `[2,3]` (comment) → `[]`.
- `textDocument/definition` at `src/service.ts` `[3,14]` (string) → `[]`.
- `textDocument/hover` at `src/service.ts` `[3,14]` (string) → `null`.
- `textDocument/references` at `src/lookalike.ts` `[3,16]`,
  `includeDeclaration: true` → exactly one result, `src/lookalike.ts`
  `[3,16]-[3,27]`. It never leaks into `user.ts`'s reference set, and
  `user.ts`'s set never leaks into it.

## 1.5 UTF-16 column handling (TS)

`src/unicode.ts` line 5 is:

```
export const ROCKETS = "🚀🚀🚀🚀🚀🚀🚀🚀"; export const rocketName = getUserById("42").name;
```

`U+1F680 ROCKET` is non-BMP: **1 codepoint, 2 UTF-16 code units, 4 UTF-8
bytes**. Eight of them sit before the interesting symbols, which is enough that
both wrong encodings land outside the target identifier (`getUserById` is only
11 characters, so fewer emoji would leave a byte-offset misread still *inside*
the identifier and the test would pass by accident).

Column of each symbol on line 5, under each encoding:

| Symbol | **UTF-16 (correct)** | Codepoint | UTF-8 byte |
|---|---|---|---|
| `ROCKETS` | **13** | 13 | 13 |
| `rocketName` | **56** | 48 | 72 |
| `getUserById` | **69** | 61 | 85 |

**The authoritative value: `getUserById` on `src/unicode.ts` line 5 starts at
UTF-16 character 69 and its range is `[5,69]-[5,80]`.**

Verified behavior at each column (this is the actual UTF-16 test):

| Request | Column | Result |
|---|---|---|
| `definition` | **69** (UTF-16) | ✅ `src/user.ts` `[5,16]-[5,27]` |
| `definition` | 61 (codepoint) | ❌ `src/unicode.ts` `[5,56]-[5,66]` — resolves `rocketName`, a *different symbol*, silently |
| `definition` | 85 (byte) | ❌ `[]` |
| `hover` | **69** | ✅ range `[5,69]-[5,80]`, `(alias) getUserById(...)` |
| `hover` | 56 | `const rocketName: string`, range `[5,56]-[5,66]` |

The codepoint case is the nasty one: it returns a **plausible wrong answer**
rather than an error. Assert the returned location, not just non-emptiness.

## 1.6 Hover (TS)

Raw `textDocument/hover` results. `contents.kind` is always `"markdown"`.
Note the leading `\n` that typescript-language-server emits.

| Position | `contents.value` | `range` |
|---|---|---|
| `src/user.ts` `[5,16]` | `` "\n```typescript\nfunction getUserById(id: string): User\n```\n" `` | `[5,16]-[5,27]` |
| `src/service.ts` `[6,21]` | `` "\n```typescript\n(alias) getUserById(id: string): User\nimport getUserById\n```\n" `` | `[6,21]-[6,32]` |
| `src/unicode.ts` `[5,69]` | `` "\n```typescript\n(alias) getUserById(id: string): User\nimport getUserById\n```\n" `` | `[5,69]-[5,80]` |
| `src/unicode.ts` `[5,56]` | `` "\n```typescript\nconst rocketName: string\n```\n" `` | `[5,56]-[5,66]` |
| `src/user.ts` `[0,17]` | `` "\n```typescript\ninterface User\n```\n" `` | `[0,17]-[0,21]` |
| `src/broken.ts` `[7,34]` | `` "\n```typescript\nany\n```\n" `` | `[7,34]-[7,45]` |
| `src/service.ts` `[3,14]` | `null` (inside a string literal) | — |

The signature line to assert on for `get_hover` at the definition is exactly:

```
function getUserById(id: string): User
```

None of the TS fixture symbols carry a doc comment, so the SPEC §4.4 Hover
`documentation` field has no source here — use the Python fixture for that
(§2.6).

## 1.7 `workspace/symbol` (TS)

| Query | Results (in order) |
|---|---|
| `"getUserById"` | `getUserById` k12 `src/lookalike.ts` `[3,0]-[5,1]`; `getUserById` k12 `src/user.ts` `[5,0]-[7,1]` |
| `"rocketName"` | `rocketName` k14 `src/unicode.ts` `[5,56]-[5,91]` |
| `"describeUser"` | `describeUser` k12 `src/service.ts` `[5,0]-[8,1]` |

`workspace/symbol` is a **fuzzy** search: query `"User"` also matches `user`,
`describeUser`, etc. Assert on containment of the expected entry, not on exact
result-set size, for fuzzy queries. `containerName` is `null` for every TS
fixture symbol.

## 1.8 Diagnostics (TS)

`src/broken.ts` holds the **only** deliberate error in the whole fixture. All
four other files publish **zero** diagnostics.

| Field | Value |
|---|---|
| path | `src/broken.ts` |
| severity | `1` (Error) |
| **code** | `2339` — **integer**, not the string `"TS2339"` |
| source | `"typescript"` |
| range | `[7,34]-[7,45]` |
| message | `Property 'missingProp' does not exist on type 'Widget'.` |

> SPEC §4.4's `Diagnostic` example renders this as `"code": "TS2339"`. That
> `TS` prefix is a **bridge-side normalization** the MCP layer must apply; the
> language server sends the bare integer `2339`. Documented here so the M1 (raw)
> and M4 (bridge shape) tests assert different things on purpose.

## 1.9 Running typescript-language-server against this fixture

**`ts-project/` deliberately has no `typescript` package installed.**
typescript-language-server locates `tsserver.js` by searching the workspace's
`node_modules` and then its own install; if it finds neither it fails
`initialize` with:

```
Could not find a valid TypeScript installation. Please ensure that the
"typescript" dependency is installed in the workspace or that a valid
`tsserver.path` is specified. Exiting.
```

The harness must therefore pass an explicit path in `initializationOptions`:

```json
{ "tsserver": { "path": "<abs>/typescript/lib/tsserver.js" } }
```

**The concrete path on this machine is:**

```
~/.local/share/langer-devtools/ts5/node_modules/typescript/lib/tsserver.js
```

Note the `ts5/` segment. `langer-devtools/node_modules/typescript` is **7.0.2**,
the native preview, whose `lib/` contains only `tsc.js` — there is no
`tsserver.js` there and pointing at it fails `initialize`. The TypeScript
**5.9.3** install under `ts5/` is the one that backs
typescript-language-server 5.3.0, and it is the one every value in this
document was captured against.

This is not an inconvenience to work around by installing TypeScript into
`ts-project/node_modules` — **the workspace-local search is exactly the
behavior SPEC §9 forbids langer from relying on.** Pointing at a
langer-controlled TypeScript keeps the tripwire meaningful.

---

# 2. `py-project/` — Python fixture

```
py-project/
├── pyrightconfig.json     # include: ["."], typeCheckingMode: "basic"
├── user.py                # THE definition site
├── service.py             # 2 cross-file references + comment/string traps
├── lookalike.py           # same NAME, different symbol (semantic trap)
├── unicode_positions.py   # 1 cross-file reference behind 8 non-BMP chars
└── broken.py              # the one deliberate type error
```

Flat layout on purpose: `from user import ...` resolves because pyright puts the
project root on the search path. No `extraPaths`, no virtualenv, no installed
packages — pyright resolves the whole fixture from source.

## 2.1 Sources with 0-based line numbers

### `user.py`

```
  0| """User model and lookup helpers."""
  1|
  2|
  3| class User:
  4|     def __init__(self, user_id: str, name: str) -> None:
  5|         self.id = user_id
  6|         self.name = name
  7|
  8|
  9| def get_user_by_id(user_id: str) -> User:
 10|     """Return a User for the given id."""
 11|     return User(user_id, "user-" + user_id)
```

### `service.py`

```
  0| """Consumers that live in a different module from the definition."""
  1|
  2| from user import User, get_user_by_id
  3|
  4| # get_user_by_id is mentioned in this comment but is not a reference.
  5| NOTE = "get_user_by_id appears in this string literal too"
  6|
  7|
  8| def describe_user(user_id: str) -> str:
  9|     user: User = get_user_by_id(user_id)
 10|     return NOTE + ": " + user.name
 11|
 12|
 13| def greet_user(user_id: str) -> str:
 14|     other = get_user_by_id(user_id)
 15|     return "Hello, " + other.name
```

### `lookalike.py`

```
  0| """A different function that merely shares the name.
  1|
  2| A textual search for "get_user_by_id" finds it; a semantic reference search
  3| rooted at user.py must NOT report it.
  4| """
  5|
  6|
  7| def get_user_by_id(user_id: str) -> str:
  8|     return "lookalike-" + user_id
```

### `unicode_positions.py`

```
  0| """UTF-16 offset fixture.
  1|
  2| Line 9 (0-based) puts eight non-BMP characters before two symbols, chosen so
  3| that a byte-offset or codepoint-offset misreading of the column lands outside
  4| the intended identifier. See testdata/README.md.
  5| """
  6|
  7| from user import get_user_by_id
  8|
  9| ROCKETS = "🚀🚀🚀🚀🚀🚀🚀🚀"; rocket_name = get_user_by_id("42").name
```

### `broken.py`

```
  0| """Deliberate type error, kept out of the reference graph on purpose."""
  1|
  2|
  3| class Widget:
  4|     def __init__(self) -> None:
  5|         self.id = "w1"
  6|
  7|
  8| widget = Widget()
  9| BROKEN_VALUE = widget.missing_prop
```

## 2.2 Symbol table (declarations)

| Symbol | Path | Kind | `selectionRange` (identifier) | `range` (full) |
|---|---|---|---|---|
| `User` | `user.py` | 5 Class | `[3,6]-[3,10]` | `[3,0]-[6,24]` |
| `User.__init__` | `user.py` | 6 Method | `[4,8]-[4,16]` | `[4,4]-[6,24]` |
| `User.id` | `user.py` | 13 Variable | `[5,13]-[5,15]` | `[5,13]-[5,15]` |
| `User.name` | `user.py` | 13 Variable | `[6,13]-[6,17]` | `[6,13]-[6,17]` |
| `get_user_by_id` | `user.py` | 12 Function | `[9,4]-[9,18]` | `[9,0]-[11,43]` |
| `NOTE` | `service.py` | 14 Constant | `[5,0]-[5,4]` | `[5,0]-[5,4]` |
| `describe_user` | `service.py` | 12 Function | `[8,4]-[8,17]` | `[8,0]-[10,34]` |
| `greet_user` | `service.py` | 12 Function | `[13,4]-[13,14]` | `[13,0]-[15,33]` |
| `get_user_by_id` (lookalike) | `lookalike.py` | 12 Function | `[7,4]-[7,18]` | `[7,0]-[8,33]` |
| `ROCKETS` | `unicode_positions.py` | 14 Constant | `[9,0]-[9,7]` | `[9,0]-[9,7]` |
| `rocket_name` | `unicode_positions.py` | 13 Variable | `[9,30]-[9,41]` | `[9,30]-[9,41]` |
| `Widget` | `broken.py` | 5 Class | `[3,6]-[3,12]` | `[3,0]-[5,22]` |
| `widget` | `broken.py` | 13 Variable | `[8,0]-[8,6]` | `[8,0]-[8,6]` |
| `BROKEN_VALUE` | `broken.py` | 14 Constant | `[9,0]-[9,12]` | `[9,0]-[9,12]` |

Pyright also emits a child of kind 13 for every parameter (e.g. `user_id`
`[9,19]-[9,31]` under `get_user_by_id`). Top-level order in `user.py` is `User`
then `get_user_by_id`; in `service.py` it is `NOTE`, `describe_user`,
`greet_user`.

## 2.3 `get_user_by_id` — the reference graph

**Definition:** `user.py` `[9,4]-[9,18]`.

`textDocument/references` at `user.py` `[9,4]` returns exactly these, in order:

| # | Path | Range | `includeDeclaration: false` too? | What it is |
|---|---|---|---|---|
| 1 | `user.py` | `[9,4]-[9,18]` | no (declaration) | the definition |
| 2 | `service.py` | `[2,23]-[2,37]` | yes | import specifier |
| 3 | `service.py` | `[9,17]-[9,31]` | yes | **cross-file call #1** |
| 4 | `service.py` | `[14,12]-[14,26]` | yes | **cross-file call #2** |
| 5 | `unicode_positions.py` | `[7,17]-[7,31]` | yes | import specifier |
| 6 | `unicode_positions.py` | `[9,44]-[9,58]` | yes | **cross-file call #3** (behind emoji) |

6 results with `includeDeclaration: true`, 5 with `false`.

**`User` references** at `user.py` `[3,6]`, `includeDeclaration: true` — 5
results in order: `user.py` `[3,6]-[3,10]`, `user.py` `[9,36]-[9,40]` (return
annotation), `user.py` `[11,11]-[11,15]` (constructor call), `service.py`
`[2,17]-[2,21]`, `service.py` `[9,10]-[9,14]`.

## 2.4 Textual-vs-semantic trap (Python)

The string `get_user_by_id` occurs **10 times** across `py-project/`. Only 6 of
them are references to `user.py`'s `get_user_by_id`:

| Path | Position | Reference? | Why |
|---|---|---|---|
| `service.py` | `[4,2]-[4,16]` | ❌ | inside a `#` comment |
| `service.py` | `[5,8]-[5,22]` | ❌ | inside a string literal |
| `lookalike.py` | `[2,22]-[2,36]` | ❌ | inside a docstring |
| `lookalike.py` | `[7,4]-[7,18]` | ❌ | a **different symbol** with the same name |

Verified negative probes:

- `definition` at `service.py` `[4,2]` (comment) → `null`.
- `definition` at `service.py` `[5,8]` (string) → `null`.
- `hover` at `service.py` `[5,8]` (string) → `null`.
- `references` at `lookalike.py` `[7,4]`, `includeDeclaration: true` → exactly
  one result, `lookalike.py` `[7,4]-[7,18]`.

Note pyright returns bare `null` where typescript-language-server returns `[]`.
The bridge must map both to `NO_RESULT` (SPEC §3.6).

## 2.5 UTF-16 column handling (Python)

`unicode_positions.py` line 9 is:

```
ROCKETS = "🚀🚀🚀🚀🚀🚀🚀🚀"; rocket_name = get_user_by_id("42").name
```

| Symbol | **UTF-16 (correct)** | Codepoint | UTF-8 byte |
|---|---|---|---|
| `ROCKETS` | **0** | 0 | 0 |
| `rocket_name` | **30** | 22 | 46 |
| `get_user_by_id` | **44** | 36 | 60 |

**The authoritative value: `get_user_by_id` on `unicode_positions.py` line 9
starts at UTF-16 character 44 and its range is `[9,44]-[9,58]`.**

| Request | Column | Result |
|---|---|---|
| `definition` | **44** (UTF-16) | ✅ `user.py` `[9,4]-[9,18]` |
| `definition` | 36 (codepoint) | ❌ `unicode_positions.py` `[9,30]-[9,41]` — resolves `rocket_name`, silently wrong |
| `definition` | 60 (byte) | ❌ `null` |
| `hover` | **44** | ✅ range `[9,44]-[9,58]` |
| `hover` | 30 | `(variable) rocket_name: str`, range `[9,30]-[9,41]` |

## 2.6 Hover (Python)

`contents.kind` is `"markdown"`. Pyright puts the docstring after a `---` rule —
this is where SPEC §4.4's Hover `documentation` field comes from.

| Position | `contents.value` | `range` |
|---|---|---|
| `user.py` `[9,4]` | `` "```python\n(function) def get_user_by_id(user_id: str) -> User\n```\n---\nReturn a User for the given id." `` | `[9,4]-[9,18]` |
| `service.py` `[9,17]` | identical to the row above | `[9,17]-[9,31]` |
| `unicode_positions.py` `[9,44]` | identical to the row above | `[9,44]-[9,58]` |
| `user.py` `[3,6]` | `` "```python\n(class) User\n```" `` | `[3,6]-[3,10]` |
| `unicode_positions.py` `[9,30]` | `` "```python\n(variable) rocket_name: str\n```" `` | `[9,30]-[9,41]` |
| `broken.py` `[9,22]` | `` "```python\n(function) missing_prop: Unknown\n```" `` | `[9,22]-[9,34]` |
| `service.py` `[5,8]` | `null` (inside a string literal) | — |

Assertable pieces for `get_hover` at the definition:

- signature: `(function) def get_user_by_id(user_id: str) -> User`
- documentation: `Return a User for the given id.`

Unlike the TS fixture, hovering a **reference** yields the same text as hovering
the **definition** (pyright does not add an `(alias)` line), so both are stable
targets.

## 2.7 `workspace/symbol` (Python)

| Query | Results (in order) |
|---|---|
| `"get_user_by_id"` | `get_user_by_id` k12 `user.py` `[9,4]-[9,18]`; `get_user_by_id` k12 `lookalike.py` `[7,4]-[7,18]` |
| `"rocket_name"` | `rocket_name` k13 `unicode_positions.py` `[9,30]-[9,41]` |
| `"describe_user"` | `describe_user` k12 `service.py` `[8,4]-[8,17]` |

Pyright's `location.range` for workspace symbols is the **identifier** range,
not the whole declaration — the opposite of typescript-language-server, which
returns the full declaration range (compare §1.7). Unlike TS, pyright **does**
populate `containerName` (e.g. `user_id` has container `get_user_by_id`).

## 2.8 Diagnostics (Python)

`broken.py` holds the only deliberate error. The other four files publish zero
diagnostics.

| Field | Value |
|---|---|
| path | `broken.py` |
| severity | `1` (Error) |
| **code** | `"reportAttributeAccessIssue"` — **string**, pyright's rule name |
| source | `"Pyright"` (capital `P`) |
| range | `[9,22]-[9,34]` |
| message | see below |

The message is two lines, and the continuation is indented with **two U+00A0
NO-BREAK SPACE characters**, not ASCII spaces:

```
Cannot access attribute "missing_prop" for class "Widget"\n  Attribute "missing_prop" is unknown
```

Match on the first line (or a prefix) rather than the whole string; the
detail line is pyright-version-sensitive.

> Note the contrast with TypeScript: the two servers disagree on the *type* of
> `Diagnostic.code` (int vs string) and on `source` capitalisation. SPEC §4.4
> types `code` as a string, so the bridge normalizes; these two fixtures are the
> minimum pair that forces that normalization to be written and tested.

---

# 3. Security tripwire (M6)

Files:

```
ts-project/node_modules/.bin/fake-server                     (Unix sh, mode 0755)
ts-project/node_modules/.bin/typescript-language-server       (Unix sh, mode 0755)
ts-project/node_modules/.bin/fake-server.cmd                  (Windows tripwire)
ts-project/node_modules/.bin/typescript-language-server.cmd   (Windows tripwire)
ts-project/node_modules/fake-language-server/bin/fake-server.sh (mode 0755)
ts-project/node_modules/fake-language-server/package.json
ts-project/node_modules/.package-lock.json
```

Unix tripwires are `/bin/sh` scripts; Windows tripwires are `.cmd` siblings so
PATHEXT-aware lookup can see them. They live inside `node_modules/` **on
purpose** — that is the entire point of SPEC §9's invariant: *opening a
workspace must never execute project-local binaries.*

## 3.1 Contract

On execution the script:

1. resolves its sentinel path as
   `${LANGER_TRIPWIRE_SENTINEL:-${TMPDIR:-/tmp}/langer-tripwire-sentinel}`;
2. **appends** one line to that file:
   `TRIPWIRE fake-server executed pid=<pid> ppid=<ppid> args=[<argv>]`;
3. writes nothing to stdout or stderr;
4. exits `0`.

It deliberately does **not** speak LSP. Anything that spawns it and waits for an
`initialize` response gets silence — a hang, not a false pass.

## 3.2 What the M6 test must do

1. Set `LANGER_TRIPWIRE_SENTINEL` to a unique path in the test's temp dir.
   Never rely on the `$TMPDIR` fallback — a stale sentinel from another run
   would produce a false failure, and the env var makes the assertion
   hermetic.
2. Assert the sentinel does not exist **before** the test.
3. Open `testdata/ts-project/` as a langer workspace and exercise it (index,
   query) exactly as a real client would.
4. Assert the sentinel **still does not exist**, and that no child process was
   ever spawned from a path under the workspace root.

## 3.3 Why two names

`fake-server` is the obvious canary. `typescript-language-server` is the
realistic one: a bridge that resolves language servers by prepending
`<workspace>/node_modules/.bin` to `PATH` — the standard Node convention, and a
tempting shortcut — would execute *that* file while looking perfectly correct in
code review. The second name is what actually catches the bug.

Per PLAN.md ground rule 5, this test is non-negotiable and must never be
weakened to pass. If it fires, the bug is in langer, not in the fixture.

## 3.4 Verified

- Both `.bin` entries are mode `0755` and, when run by hand, write the sentinel
  and exit `0` — the tripwire genuinely works.
- The `$TMPDIR` fallback resolves correctly when `LANGER_TRIPWIRE_SENTINEL` is
  unset.
- Neither file is covered by `.gitignore`, so they survive checkout
  (`git check-ignore` exits 1 for both).
- Driving a real typescript-language-server through a full open/index/query
  cycle over `ts-project/` with `LANGER_TRIPWIRE_SENTINEL` set left the sentinel
  **absent** — the fixture does not self-trigger.

---

# 4. Provenance

Every position, range, hover string, and diagnostic above was captured from a
real language server over stdio on 2026-07-25, macOS arm64:

| Tool | Version | Invocation |
|---|---|---|
| typescript-language-server | 5.3.0 | `typescript-language-server --stdio`, `initializationOptions.tsserver.path` → TypeScript **5.9.3** |
| pyright-langserver | 1.1.411 | `pyright-langserver --stdio` |

Installed outside the repo at
`~/.local/share/langer-devtools/node_modules/.bin/` (see §1.9 for the
TypeScript 5 requirement — the devtools install also carries TypeScript 7.0.2,
the native preview, which ships **no `tsserver.js`** and cannot back
typescript-language-server 5.3.0). TypeScript 5.9.3 lives in a sibling install
at `~/.local/share/langer-devtools/ts5/node_modules/typescript/`; that is the
`tsserver.path` §1.9 requires.

Client capabilities advertised during capture: `general.positionEncodings:
["utf-16"]`, `hover.contentFormat: ["markdown", "plaintext"]`,
`documentSymbol.hierarchicalDocumentSymbolSupport: true`.

## 4.1 Re-verifying

If a value here is ever contested, reproduce it rather than reasoning about it:

1. Spawn the server with the workspace root as CWD and `rootUri` set to it.
2. `initialize` (with `tsserver.path` for TS), then `initialized`.
3. `textDocument/didOpen` every fixture file, then wait ~5–10 s for pyright to
   finish its first analysis pass — diagnostics are server-pushed and arrive
   late (SPEC §4.3's settle rule exists for exactly this reason).
4. Issue the request and compare against the table above.

Keep such scripts **outside** the repo. The only things that belong in
`testdata/` are the fixtures and this document.

## 4.2 Editing rules

These fixtures are load-bearing test data, not sample code.

- **Never insert or delete a line** without re-verifying every table here —
  the whole file is line-number-indexed.
- `src/broken.ts` and `broken.py` must remain the **only** sources of
  diagnostics; adding a second error breaks "exactly one diagnostic" assertions.
- `unicode.ts` line 5 and `unicode_positions.py` line 9 must keep all eight
  emoji. Fewer, and a byte-offset misread lands *inside* the identifier and the
  UTF-16 test passes for the wrong reason.
- Comments in the fixtures reference their own 0-based line numbers; keep them
  in sync.
