// Decide whether a paste carries files to upload or text for the terminal.
// Office apps put both a picture and the text on the clipboard; text wins
// there. A screenshot, or a file copied in Finder/Explorer, has no text (or
// only the file name) and is uploaded.
export function pasteFiles(data: DataTransfer | null): File[] {
  if (!data) return [];
  const files = Array.from(data.files ?? []);
  if (files.length === 0) {
    for (const item of Array.from(data.items ?? [])) {
      const file = item.kind === "file" ? item.getAsFile() : null;
      if (file) files.push(file);
    }
  }
  if (files.length === 0) return [];
  const text = (data.getData("text/plain") ?? "").trim();
  if (text === "" || files.some((file) => file.name === text)) return files;
  return [];
}

// insertPaths turns uploaded paths into terminal input. The device names files
// with [A-Za-z0-9._-] only, so no shell quoting is needed. Bracketed paste
// lets tools such as Claude Code treat the path as a pasted attachment.
export function insertPaths(paths: string[], bracketed: boolean): string {
  const text = paths.join(" ") + " ";
  return bracketed ? `\x1b[200~${text}\x1b[201~` : text;
}
