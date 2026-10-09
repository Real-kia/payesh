<script context="module" lang="ts">
  let modalCounter = 0;
</script>

<script lang="ts">
  import Icon from './Icon.svelte';
  import { onDestroy, tick } from 'svelte';

  export let open: boolean = false;
  export let title: string = '';
  export let description: string = '';
  export let tone: 'danger' | 'warning' | 'info' | 'primary' = 'primary';
  export let icon: string = '';
  export let confirmText: string = 'Confirm';
  export let cancelText: string = 'Cancel';
  export let hideCancel: boolean = false;
  export let busy: boolean = false;
  export let confirmDisabled: boolean = false;
  export let onConfirm: () => void = () => {};
  export let onCancel: () => void = () => {};

  const titleId = `payesh-modal-title-${++modalCounter}`;
  const descriptionId = `${titleId}-description`;
  let dialog: HTMLDialogElement | undefined;
  let pointerStartedOutside = false;

  function cancel() {
    if (!busy) onCancel();
  }

  function handleCancel(event: Event) {
    // Keep the native dialog and the controlled open prop in sync. This also
    // blocks Escape while an operation is in progress.
    event.preventDefault();
    cancel();
  }

  function focusInitialControl() {
    if (!dialog) return;
    const cancelButton = dialog.querySelector<HTMLButtonElement>('.modal-cancel-button:not(:disabled)');
    const bodyControls = Array.from(dialog.querySelectorAll<HTMLElement>(
      '.modal-body input:not([type="hidden"]):not(:disabled), .modal-body select:not(:disabled), .modal-body textarea:not(:disabled), .modal-body button:not(:disabled), .modal-body a[href], .modal-body [tabindex]:not([tabindex="-1"]):not(:disabled)'
    ));
    const bodyControl = bodyControls.find(control => control.getClientRects().length > 0);
    const fallback = dialog.querySelector<HTMLButtonElement>('.modal-confirm-button:not(:disabled), .modal-close-button:not(:disabled)');
    (cancelButton ?? bodyControl ?? fallback ?? dialog).focus();
  }

  async function openDialog() {
    // Wait for conditional slot/buttons to be mounted before native focus
    // selection. The prop can change again while this update is pending.
    await tick();
    if (!open || !dialog?.isConnected || dialog.open) return;
    dialog.showModal();
    focusInitialControl();
  }

  $: if (dialog) {
    if (open && !dialog.open) {
      // Native modal dialogs make the surrounding document inert, contain
      // keyboard focus, and restore focus to the trigger when closed.
      void openDialog();
    } else if (!open && dialog.open) {
      dialog.close();
    }
  }

  onDestroy(() => {
    if (dialog?.open) dialog.close();
  });
</script>

<dialog
  bind:this={dialog}
  class="modal-backdrop"
  aria-labelledby={titleId}
  aria-describedby={description ? descriptionId : undefined}
  aria-busy={busy}
  on:cancel={handleCancel}
  on:pointerdown={(event) => { pointerStartedOutside = event.target === dialog; }}
  on:click={(event) => { if (event.target === dialog && pointerStartedOutside) cancel(); }}
>
  {#if open}
    <div
      class={`modal-card ${tone}`}
    >
      <!-- CLOSE BUTTON -->
      <button class="modal-close-button" type="button" aria-label="Close dialog" disabled={busy} on:click={cancel}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
          <line x1="18" y1="6" x2="6" y2="18"></line>
          <line x1="6" y1="6" x2="18" y2="18"></line>
        </svg>
      </button>

      <!-- ICON & HEADER -->
      <div class="modal-header">
        {#if icon}
          <div class={`modal-icon-badge ${tone}`}>
            <Icon name={icon} size={20} />
          </div>
        {/if}
        <div class="modal-title-group">
          <h3 id={titleId} class="modal-title">{title}</h3>
          {#if description}
            <p id={descriptionId} class="modal-desc">{description}</p>
          {/if}
        </div>
      </div>

      <!-- CUSTOM SLOT CONTENT -->
      <div class="modal-body">
        <slot />
      </div>

      <!-- ACTIONS -->
      <div class="modal-actions">
        {#if !hideCancel}
          <button class="button ghost modal-cancel-button" type="button" disabled={busy} on:click={cancel}>
            {cancelText}
          </button>
        {/if}
        <button
          class={`button modal-confirm-button ${tone === 'danger' ? 'danger' : 'primary'}`}
          type="button"
          disabled={busy || confirmDisabled}
          on:click={onConfirm}
        >
          {#if busy}
            <Icon name="loader" size={14} class="spin" />
          {/if}
          <span>{confirmText}</span>
        </button>
      </div>
    </div>
  {/if}
</dialog>

<style>
  .modal-backdrop {
    position: fixed;
    inset: 0;
    width: 100%;
    height: 100%;
    max-width: none;
    max-height: none;
    margin: 0;
    border: 0;
    background: transparent;
    color: inherit;
    padding: 1.5rem;
    box-sizing: border-box;
    overflow: auto;
  }

  .modal-backdrop[open] {
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .modal-backdrop::backdrop {
    background: rgba(3, 7, 18, 0.75);
    backdrop-filter: blur(8px);
    -webkit-backdrop-filter: blur(8px);
    animation: backdrop-fade 0.18s ease-out;
  }

  @keyframes backdrop-fade {
    from { opacity: 0; }
    to { opacity: 1; }
  }

  .modal-card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 14px;
    box-shadow: 0 24px 64px -12px rgba(0, 0, 0, 0.7), 0 0 0 1px rgba(255, 255, 255, 0.05);
    width: 100%;
    max-width: 460px;
    flex-shrink: 0;
    max-height: 100%;
    padding: 1.5rem;
    position: relative;
    overflow: auto;
    display: flex;
    flex-direction: column;
    gap: 1.25rem;
    animation: modal-scale 0.22s cubic-bezier(0.16, 1, 0.3, 1);
  }

  @keyframes modal-scale {
    from {
      opacity: 0;
      transform: scale(0.95) translateY(6px);
    }
    to {
      opacity: 1;
      transform: scale(1) translateY(0);
    }
  }

  /* TOP GLOW ACCENT LINE */
  .modal-card::before {
    content: '';
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    height: 2px;
  }

  .modal-card.danger::before {
    background: linear-gradient(90deg, #ef4444, #f87171);
  }
  .modal-card.warning::before {
    background: linear-gradient(90deg, #f59e0b, #fbbf24);
  }
  .modal-card.info::before,
  .modal-card.primary::before {
    background: linear-gradient(90deg, #06b6d4, #3b82f6);
  }

  .modal-close-button {
    position: absolute;
    top: 1rem;
    right: 1rem;
    background: transparent;
    border: none;
    color: var(--ink-faint);
    width: 28px;
    height: 28px;
    border-radius: 6px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    cursor: pointer;
    transition: all 0.15s ease;
  }

  .modal-close-button:hover {
    background: var(--surface-muted);
    color: var(--ink);
  }

  .modal-header {
    display: flex;
    align-items: flex-start;
    gap: 1rem;
    padding-right: 1.5rem;
  }

  .modal-icon-badge {
    width: 44px;
    height: 44px;
    border-radius: 10px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
  }

  .modal-icon-badge.danger {
    background: rgba(239, 68, 68, 0.12);
    color: #ef4444;
    border: 1px solid rgba(239, 68, 68, 0.25);
  }

  .modal-icon-badge.warning {
    background: rgba(245, 158, 11, 0.12);
    color: #f59e0b;
    border: 1px solid rgba(245, 158, 11, 0.25);
  }

  .modal-icon-badge.info,
  .modal-icon-badge.primary {
    background: rgba(6, 182, 212, 0.12);
    color: #06b6d4;
    border: 1px solid rgba(6, 182, 212, 0.25);
  }

  .modal-title-group {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }

  .modal-title {
    margin: 0;
    font-size: 1.125rem;
    font-weight: 600;
    letter-spacing: -0.015em;
    color: var(--ink);
  }

  .modal-desc {
    margin: 0;
    font-size: 0.84375rem;
    line-height: 1.45;
    color: var(--ink-secondary);
    white-space: pre-line;
  }

  .modal-body {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }

  .modal-actions {
    display: flex;
    justify-content: flex-end;
    align-items: center;
    gap: 0.75rem;
    padding-top: 0.5rem;
  }

  .button {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 8px;
    padding: 8px 16px;
    border-radius: var(--radius-md, 5px);
    border: 1px solid transparent;
    font-size: 13px;
    font-weight: 500;
    cursor: pointer;
    transition: all 0.15s ease;
    text-decoration: none;
    line-height: 1.25;
    font-family: inherit;
  }

  .button:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }

  .button.ghost {
    background: transparent;
    border-color: var(--line, rgba(255, 255, 255, 0.12));
    color: var(--ink-secondary, #94a3b8);
  }

  .button.ghost:hover:not(:disabled) {
    background: var(--surface-muted, rgba(255, 255, 255, 0.06));
    border-color: var(--border, rgba(255, 255, 255, 0.2));
    color: var(--ink, #f1f5f9);
  }

  .button.primary {
    background: var(--teal);
    border-color: var(--teal);
    color: #ffffff;
    box-shadow: none;
    font-weight: 600;
  }

  .button.primary:hover:not(:disabled) {
    filter: brightness(1.1);
  }

  .button.danger {
    background: var(--danger);
    border-color: var(--danger);
    color: #ffffff;
    box-shadow: none;
    font-weight: 600;
  }

  .button.danger:hover:not(:disabled) {
    filter: brightness(1.1);
  }

  :global(.spin) {
    animation: spin 0.8s linear infinite;
  }

  @keyframes spin {
    from { transform: rotate(0deg); }
    to { transform: rotate(360deg); }
  }
</style>
