import * as RadixDialog from "@radix-ui/react-dialog";
import type { ReactNode } from "react";

export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  onOpenAutoFocus,
  children,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: string;
  /** Call preventDefault() to choose the initially focused element yourself. */
  onOpenAutoFocus?: (event: Event) => void;
  children: ReactNode;
}) {
  return (
    <RadixDialog.Root open={open} onOpenChange={onOpenChange}>
      <RadixDialog.Portal>
        <RadixDialog.Overlay className="fixed inset-0 bg-black/40" />
        <RadixDialog.Content onOpenAutoFocus={onOpenAutoFocus} className="fixed top-1/2 left-1/2 w-[min(28rem,92vw)] -translate-x-1/2 -translate-y-1/2 rounded-ui border border-line bg-surface p-5 shadow-xl">
          <RadixDialog.Title className="text-base font-semibold">{title}</RadixDialog.Title>
          <RadixDialog.Description className="mt-1 text-sm text-muted">
            {description}
          </RadixDialog.Description>
          <div className="mt-4">{children}</div>
        </RadixDialog.Content>
      </RadixDialog.Portal>
    </RadixDialog.Root>
  );
}
