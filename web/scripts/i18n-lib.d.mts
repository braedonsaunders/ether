export interface Message {
  one?: string;
  refs: string[];
}
export function extract(files?: string[]): { messages: Map<string, Message>; dynamic: string[] };
export function localeCodes(): string[];
export function checkCatalog(
  locale: string,
  messages: Map<string, Message>,
): { missing: string[]; stale: string[]; invalid: string[] };
export function sourceCatalogIsCurrent(messages: Map<string, Message>): boolean;
export function staleMemos(files?: string[]): string[];
