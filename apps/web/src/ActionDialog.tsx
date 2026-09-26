import { FormEvent, ReactNode, useEffect, useRef } from "react";

type Props = {
  title: string;
  description?: string;
  confirmLabel: string;
  destructive?: boolean;
  busy?: boolean;
  error?: string;
  onClose: () => void;
  onSubmit: () => void;
  children?: ReactNode;
};

export default function ActionDialog({ title, description, confirmLabel, destructive = false, busy = false, error, onClose, onSubmit, children }: Props) {
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    dialog.showModal();
    dialog.querySelector<HTMLElement>("input, select, .dialog-cancel")?.focus();
    return () => dialog.close();
  }, []);

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!busy) onSubmit();
  }

  return <dialog ref={ref} className="action-dialog" aria-labelledby="action-dialog-title" aria-describedby={description ? "action-dialog-description" : undefined}
    onCancel={(event) => { event.preventDefault(); if (!busy) onClose(); }}
    onClick={(event) => { if (event.target === ref.current && !busy) onClose(); }}>
    <form noValidate onSubmit={submit}>
      <h2 id="action-dialog-title">{title}</h2>
      {description && <p id="action-dialog-description" className="dialog-description">{description}</p>}
      {children && <div className="dialog-fields">{children}</div>}
      {error && <p className="dialog-error" role="alert">{error}</p>}
      <div className="dialog-actions">
        <button className="dialog-cancel" type="button" disabled={busy} onClick={onClose}>取消</button>
        <button className={destructive ? "dialog-confirm destructive" : "dialog-confirm"} type="submit" disabled={busy}>{busy ? "处理中…" : confirmLabel}</button>
      </div>
    </form>
  </dialog>;
}
