<script lang="ts">
  let { state, label = '' }: { state: string; label?: string } = $props();
</script>

<span class={`status-pill ${state}`}>
  <i class="status-dot" aria-hidden="true"></i>
  <span>{label || state.replace('-', ' ')}</span>
</span>
