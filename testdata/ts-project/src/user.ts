export interface User {
  id: string;
  name: string;
}

export function getUserById(id: string): User {
  return { id, name: "user-" + id };
}
