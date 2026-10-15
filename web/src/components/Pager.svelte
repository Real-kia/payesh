<script lang="ts">
  let { total, page = $bindable(0), pageSize, label = 'items' }: {
    total: number;
    page: number;
    pageSize: number;
    label?: string;
  } = $props();

  const pageCount = $derived(Math.max(1, Math.ceil(total / pageSize)));
  $effect(() => { if (page > pageCount - 1) page = pageCount - 1; });
</script>

{#if total > pageSize}
  <div class="pager">
    <span class="muted tabular">{page * pageSize + 1}–{Math.min(total, (page + 1) * pageSize)} of {total} {label}</span>
    <div class="pager-buttons">
      <button class="button ghost small" type="button" disabled={page === 0} onclick={() => page--}>Previous</button>
      <span class="muted tabular">Page {page + 1} of {pageCount}</span>
      <button class="button ghost small" type="button" disabled={page >= pageCount - 1} onclick={() => page++}>Next</button>
    </div>
  </div>
{/if}

<style>
  .pager { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; margin-top: 12px; font-size: 12.5px; }
  .pager-buttons { display: flex; align-items: center; gap: 10px; }
</style>
