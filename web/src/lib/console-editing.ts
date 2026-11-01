/** True while a save bar has unsaved edits. SaveBarHost is the only writer. */
let saveBarDirty = false;

export function setSaveBarDirty(dirty: boolean): void {
  saveBarDirty = dirty;
}

/**
 * The owner is in the middle of something: a field is focused, a control
 * differs from what was rendered, or a save bar is waiting.
 */
export function consoleEditing(): boolean {
  if (saveBarDirty) return true;
  const active = document.activeElement;
  if (isTextEntry(active)) return true;
  for (const el of document.querySelectorAll("input, textarea")) {
    if (fieldDirty(el)) return true;
  }
  return false;
}

function isTextEntry(el: Element | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  if (el.isContentEditable) return true;
  if (el instanceof HTMLTextAreaElement) return true;
  if (el instanceof HTMLSelectElement) return true;
  if (el instanceof HTMLInputElement) {
    return el.type !== "hidden" && el.type !== "button" && el.type !== "submit";
  }
  return false;
}

function fieldDirty(el: Element): boolean {
  if (el instanceof HTMLTextAreaElement) return el.value !== el.defaultValue;
  if (!(el instanceof HTMLInputElement) || el.type === "hidden") return false;
  if (el.type === "checkbox" || el.type === "radio") return el.checked !== el.defaultChecked;
  if (el.type === "button" || el.type === "submit") return false;
  return el.value !== el.defaultValue;
}
