import type { ComponentProps, ReactNode } from "react";
import { Drawer } from "../../components/Drawer";
import { t } from "../../i18n";

/** Collections and channel programming share one preview-and-conversation surface. */
export function DescribeSurface({
  preview,
  error,
  children,
  ...props
}: Omit<ComponentProps<typeof Drawer>, "size" | "bodyClassName"> & {
  preview: ReactNode;
  error?: string;
}) {
  return (
    <Drawer {...props} size="xl" bodyClassName="flex min-h-0 flex-col overflow-hidden p-0">
      <section
        className="relative min-h-0 flex-[2] basis-0 overflow-auto border-b border-border"
        aria-label={t("Live build")}
      >
        {preview}
      </section>
      {error ? (
        <p role="alert" className="shrink-0 border-b border-border px-4 py-2 text-sm text-danger">
          {error}
        </p>
      ) : null}
      <div className="describe-chat flex min-h-0 flex-1 basis-0 flex-col">{children}</div>
    </Drawer>
  );
}

/**
 * Puts an example into the agent panel's composer so the owner can send or
 * edit it. The composer keeps its own state, so this goes through the input
 * event React listens for.
 */
export function fillComposer(root: HTMLElement | null, text: string) {
  const box = root?.querySelector("textarea");
  if (!box) return;
  Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")?.set?.call(box, text);
  box.dispatchEvent(new Event("input", { bubbles: true }));
  box.focus();
}
