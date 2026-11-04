/** Keep native new-tab/window behavior while routing ordinary clicks locally. */
export function followLink(event: MouseEvent, navigate: () => void): void {
  if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  event.preventDefault();
  navigate();
}
