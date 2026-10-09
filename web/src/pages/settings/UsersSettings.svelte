<script lang="ts">
  import Icon from '../../Icon.svelte';
  import { apiClient, type Account } from '../../api';
  import type { ConfirmOptions } from '../../lib/dialogs';

  let { accounts, currentUser, onChanged, onSaved, onConfirm, onNotice, onOwnPasswordChanged }: {
    accounts: Account[];
    currentUser: string;
    onChanged: () => Promise<void>;
    onSaved: (title: string, description: string) => void;
    onConfirm: (options: ConfirmOptions) => void;
    onNotice: (message: string) => void;
    onOwnPasswordChanged: () => void;
  } = $props();

  let search = $state('');
  let adding = $state(false);
  let busy = $state(false);
  let error = $state('');

  let newUsername = $state('');
  let newPassword = $state('');
  let newRole = $state<'admin' | 'member'>('member');
  let newPermission = $state<'read' | 'edit'>('read');

  let editing = $state('');
  let editUsername = $state('');
  let editPassword = $state('');
  let editRole = $state<'owner' | 'admin' | 'member'>('member');
  let editPermission = $state<'read' | 'edit'>('read');

  const visible = $derived(accounts.filter((account) => account.username.toLowerCase().includes(search.trim().toLowerCase())));

  async function create(): Promise<void> {
    if (!newUsername.trim() || !newPassword || busy) return;
    busy = true; error = '';
    try {
      const created = newUsername.trim();
      await apiClient.createAccount({ username: created, password: newPassword, role: newRole, permission: newPermission });
      newUsername = ''; newPassword = ''; adding = false;
      await onChanged();
      onNotice('User added.');
      onSaved('User created', `User "${created}" was created with role "${newRole}" and ${newPermission === 'read' ? 'read-only' : 'read/write'} access.`);
    } catch (err) { error = err instanceof Error ? err.message : 'Could not add account.'; }
    finally { busy = false; }
  }

  function startEditing(account: Account): void {
    editing = account.username; editUsername = account.username; editPassword = '';
    editRole = account.role; editPermission = account.permission; error = '';
  }

  async function saveEdit(account: Account): Promise<void> {
    if (busy) return;
    busy = true; error = '';
    try {
      await apiClient.updateAccount(account.username, { username: editUsername.trim(), ...(editPassword ? { password: editPassword } : {}), ...(account.role === 'owner' ? {} : { role: editRole, permission: editPermission }) });
      const changedOwnPassword = account.role === 'owner' && editPassword !== '';
      const updated = editUsername.trim();
      editing = ''; editPassword = '';
      if (changedOwnPassword) {
        onOwnPasswordChanged();
      } else {
        await onChanged();
        onNotice('User updated.');
        onSaved('User updated', `User "${updated}" has been updated successfully.`);
      }
    } catch (err) { error = err instanceof Error ? err.message : 'Could not update account.'; }
    finally { busy = false; }
  }

  function remove(account: Account): void {
    onConfirm({ title: `Remove ${account.username}?`, description: 'Their active sessions will be revoked.', tone: 'warning', icon: 'alert-triangle', confirmText: 'Remove user', cancelText: 'Cancel', action: async () => {
      error = '';
      try {
        await apiClient.deleteAccount(account.username);
        await onChanged();
        onNotice('User removed.');
        onSaved('User removed', `User "${account.username}" has been removed.`);
      } catch (err) { error = err instanceof Error ? err.message : 'Could not remove user.'; }
    } });
  }
</script>

<article class="panel">
  <div class="panel-heading">
    <div class="title"><span class="section-icon"><Icon name="users" size={16} /></span><div><h2>User management</h2><span class="muted">{accounts.length} {accounts.length === 1 ? 'user' : 'users'}</span></div></div>
    <button class={`button small ${adding ? 'ghost' : 'primary'}`} type="button" onclick={() => (adding = !adding)}>{#if adding}Cancel{:else}<Icon name="plus" size={13} />Add user{/if}</button>
  </div>

  {#if adding}
    <form class="form-grid new-user" onsubmit={(event) => { event.preventDefault(); void create(); }}>
      <h3 class="form-wide">New user</h3>
      <label>Username<input bind:value={newUsername} minlength="3" maxlength="128" required autocomplete="off" /></label>
      <label><span id="new-password-label">Password</span><input type="password" bind:value={newPassword} minlength="12" required autocomplete="new-password" aria-labelledby="new-password-label" /><small>At least 12 characters.</small></label>
      <label>Role<select bind:value={newRole}><option value="member">Member</option><option value="admin">Admin</option></select></label>
      <label>Permission<select bind:value={newPermission}><option value="read">Read only</option><option value="edit">Can edit</option></select></label>
      <div class="setup-actions"><button class="button primary" type="submit" disabled={busy}>{busy ? 'Creating…' : 'Create user'}</button></div>
    </form>
  {/if}

  <label class="search"><Icon name="search" size={14} /><input type="search" bind:value={search} placeholder="Search by username" aria-label="Search users" /></label>
  <div class="table">
    <div class="head"><span>User</span><span>Role</span><span>Access</span><span></span></div>
    {#each visible as account (account.username)}
      <div class="row">
        <span class="who"><span class="avatar" aria-hidden="true">{account.username.slice(0, 1).toUpperCase()}</span><strong>{account.username}</strong>{#if account.username === currentUser}<span class="you">You</span>{/if}</span>
        <span class={`role ${account.role}`}>{account.role}</span>
        <span class="muted">{account.permission === 'read' ? 'Read only' : 'Can edit'}</span>
        <span class="actions">
          <button class="button ghost small" type="button" onclick={() => (editing === account.username ? (editing = '') : startEditing(account))}>{editing === account.username ? 'Close' : 'Edit'}</button>
          {#if account.role !== 'owner'}<button class="button ghost small" type="button" onclick={() => remove(account)}>Remove</button>{/if}
        </span>
      </div>
      {#if editing === account.username}
        <form class="form-grid edit" onsubmit={(event) => { event.preventDefault(); void saveEdit(account); }}>
          <label>Username<input bind:value={editUsername} minlength="3" maxlength="128" required autocomplete="off" /></label>
          <label><span id="edit-password-label">New password</span><input type="password" bind:value={editPassword} minlength="12" placeholder="Leave blank to keep current" autocomplete="new-password" aria-labelledby="edit-password-label" /></label>
          {#if account.role !== 'owner'}
            <label>Role<select bind:value={editRole}><option value="member">Member</option><option value="admin">Admin</option></select></label>
            <label>Permission<select bind:value={editPermission}><option value="read">Read only</option><option value="edit">Can edit</option></select></label>
          {/if}
          <div class="setup-actions"><button class="button ghost small" type="button" onclick={() => (editing = '')}>Cancel</button><button class="button primary small" type="submit" disabled={busy}>Save changes</button></div>
        </form>
      {/if}
    {:else}
      <p class="muted none">No users match “{search}”.</p>
    {/each}
  </div>
  {#if error}<p class="form-error" role="alert">{error}</p>{/if}
</article>

<style>
  .new-user, .edit { padding: 16px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); animation: rise-in var(--dur) var(--ease-out); }
  .new-user h3 { margin: 0; }
  .search { display: flex; align-items: center; gap: 8px; margin-bottom: 14px; padding-left: 12px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); color: var(--muted); }
  .search:focus-within { border-color: var(--accent); box-shadow: var(--ring); }
  .search input { flex: 1; border: 0; background: transparent; box-shadow: none; padding-left: 0; }
  .search input:focus { box-shadow: none; }
  .head, .row { display: grid; grid-template-columns: minmax(140px, 1fr) 90px 100px 150px; gap: 12px; align-items: center; }
  .head { padding: 0 0 8px; border-bottom: 1px solid var(--line); color: var(--muted); font-size: 11.5px; font-weight: 600; letter-spacing: 0.03em; text-transform: uppercase; }
  .row { padding: 12px 0; border-bottom: 1px solid var(--line-light); font-size: 13px; }
  .who { display: inline-flex; align-items: center; gap: 10px; min-width: 0; }
  .who strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .avatar { display: grid; flex-shrink: 0; place-items: center; width: 28px; height: 28px; border-radius: 50%; background: var(--accent-bg); color: var(--accent); font-size: 12px; font-weight: 700; }
  .you { padding: 1px 6px; border-radius: var(--radius-pill); background: var(--surface-muted); color: var(--muted); font-size: 10.5px; font-weight: 600; }
  .role { justify-self: start; padding: 2px 8px; border-radius: var(--radius-pill); background: var(--surface-muted); color: var(--ink-secondary); font-size: 11.5px; font-weight: 600; text-transform: capitalize; }
  .role.owner { background: var(--accent-bg); color: var(--accent); }
  .actions { display: flex; justify-content: flex-end; gap: 6px; }
  .edit { margin: 4px 0 12px; }
  .none { padding: 16px 0; }
  @media (max-width: 760px) {
    .head { display: none; }
    .row { grid-template-columns: 1fr auto; }
    .actions { grid-column: 1 / -1; justify-content: flex-start; }
  }
</style>
