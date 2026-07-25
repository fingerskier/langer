// A DIFFERENT function that merely shares the name. A textual search for
// "getUserById" finds it; a semantic reference search rooted at
// src/user.ts must NOT report it.
export function getUserById(id: string): string {
  return "lookalike-" + id;
}
