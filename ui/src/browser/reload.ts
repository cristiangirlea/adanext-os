// The browser shell's answer to a session that ended or is not this page's: start over at /.
export function reloadToStart(): void {
  window.location.assign("/");
}
