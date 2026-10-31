/** A stored secret is present when the server sent a placeholder or a real value. */
export function secretConfigured(value: unknown): boolean {
  return String(value ?? "").trim() !== "";
}
