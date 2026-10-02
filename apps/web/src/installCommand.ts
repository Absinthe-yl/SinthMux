// Commands that install or repair the device connector. macOS/Linux run the
// shell script; Windows runs the PowerShell script. Both fetch tmux if missing.

export type InstallOS = "unix" | "windows";

export const installOSOptions: { id: InstallOS; label: string }[] = [
  { id: "unix", label: "macOS / Linux" },
  { id: "windows", label: "Windows" }
];

function shellQuote(value: string): string {
  return "'" + value.replaceAll("'", "'\\''") + "'";
}

function powerShellQuote(value: string): string {
  return "'" + value.replaceAll("'", "''") + "'";
}

/** Install command with a pairing code, or a repair command when code is omitted. */
export function installCommand(os: InstallOS, hub: string, code?: string): string {
  if (os === "windows") {
    const args = code ? `-Hub ${powerShellQuote(hub)} -Code ${powerShellQuote(code)}` : `-Hub ${powerShellQuote(hub)} -Repair`;
    return `& ([scriptblock]::Create((irm ${powerShellQuote(`${hub}/install/connector.ps1`)}))) ${args}`;
  }
  const curlOptions = hub.startsWith("https://") ? " --proto '=https' --proto-redir '=https'" : "";
  const args = code ? `--hub ${shellQuote(hub)} --code ${shellQuote(code)}` : `--hub ${shellQuote(hub)} --repair`;
  return `curl -fsSL${curlOptions} ${shellQuote(`${hub}/install/connector.sh`)} | bash -s -- ${args}`;
}

export function guessInstallOS(): InstallOS {
  return /Windows/i.test(window.navigator.userAgent) ? "windows" : "unix";
}

/** The device's own platform, as reported by its connector ("windows", "darwin", "linux"). */
export function deviceInstallOS(platform?: string): InstallOS {
  return platform === "windows" ? "windows" : "unix";
}
