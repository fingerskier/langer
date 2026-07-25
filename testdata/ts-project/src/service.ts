import { getUserById, User } from "./user";

// getUserById is mentioned in this comment but is not a reference.
const NOTE = "getUserById appears in this string literal too";

export function describeUser(id: string): string {
  const user: User = getUserById(id);
  return NOTE + ": " + user.name;
}

export function greetUser(id: string): string {
  const other = getUserById(id);
  return "Hello, " + other.name;
}
