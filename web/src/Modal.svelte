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
    padding: 1.5rem;
    overflow: auto;
    border: 0;
    background: transparent;
    color: inherit;
  }
  .modal-backdrop[open] { display: flex; align-items: center; justify-content: center; }
  .modal-backdrop::backdrop { background: rgba(5, 9, 15, 0.55); animation: fade-in var(--dur) var(--ease-out); }
  :global(:root[data-theme='dark']) .modal-backdrop::backdrop { background: rgba(0, 0, 0, 0.6); }

  .modal-card {
    --tone: var(--accent);
    --tone-bg: var(--accent-bg);
    position: relative;
    display: flex;
    flex-direction: column;
    flex-shrink: 0;
    gap: 18px;
    width: 100%;
    max-width: 460px;
    max-height: 100%;
    padding: 24px;
    overflow: auto;
    border: 1px solid var(--line);
    border-radius: var(--radius-xl);
    background: var(--surface-elevated);
    box-shadow: var(--shadow-lg);
    animation: modal-in var(--dur-slow) var(--ease-spring);
  }
  .modal-card.danger { --tone: var(--danger); --tone-bg: var(--danger-bg); }
  .modal-card.warning { --tone: var(--warning); --tone-bg: var(--warning-bg); }
  .modal-card.info { --tone: var(--info); --tone-bg: var(--info-bg); }
  @keyframes modal-in { from { opacity: 0; transform: scale(0.96) translateY(8px); } to { opacity: 1; transform: none; } }
  .modal-card::before { content: ''; position: absolute; top: 0; left: 0; right: 0; height: 3px; background: var(--tone); opacity: 0.9; }

  .modal-close-button {
    position: absolute;
    top: 14px;
    right: 14px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 30px;
    height: 30px;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--muted);
    transition: background-color var(--transition-fast), color var(--transition-fast);
  }
  .modal-close-button:hover { background: var(--surface-muted); color: var(--ink); }

  .modal-header { display: flex; align-items: flex-start; gap: 14px; padding-right: 24px; }
  .modal-icon-badge { display: inline-flex; flex-shrink: 0; align-items: center; justify-content: center; width: 42px; height: 42px; border-radius: var(--radius-lg); background: var(--tone-bg); color: var(--tone); }
  .modal-title-group { display: flex; flex-direction: column; gap: 6px; padding-top: 2px; }
  .modal-title { margin: 0; color: var(--ink); font-size: 17px; font-weight: 650; letter-spacing: -0.015em; }
  .modal-desc { margin: 0; color: var(--ink-secondary); font-size: 13.5px; line-height: 1.5; white-space: pre-line; }
  .modal-body { display: flex; flex-direction: column; gap: 12px; }
  .modal-body:empty { display: none; }
  .modal-actions { display: flex; align-items: center; justify-content: flex-end; gap: 10px; padding-top: 4px; }
  .button.danger { background: var(--danger); border-color: var(--danger); color: #fff; }
  .button.danger:hover:not(:disabled) { filter: brightness(1.08); background: var(--danger); }
  :global(.spin) { animation: spin 0.8s linear infinite; }
  @media (max-width: 480px) {
    .modal-backdrop { align-items: flex-end !important; padding: 12px; }
    .modal-actions { flex-direction: column-reverse; align-items: stretch; }
  }
</style>
