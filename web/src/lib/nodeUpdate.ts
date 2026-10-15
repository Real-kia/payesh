export type NodeUpdateTrack = {
  serverId: string;
  jobId: string;
  /** Release being installed; nodes report it as their version once restarted. */
  release: string;
  /** Node agent version before the update started. */
  from: string;
  startedAt: number;
  jobState: string;
  error: string;
  sawOffline: boolean;
};

export type UpdatePhase = 'queued' | 'updating' | 'restarting' | 'verifying' | 'done' | 'failed' | 'stalled';

export const STALL_AFTER_MS = 10 * 60 * 1000;
export const phaseSteps: Array<{ phase: UpdatePhase; label: string; detail: string }> = [
  { phase: 'queued', label: 'Queued', detail: 'The hub accepted the update and is waiting for the node to pick it up.' },
  { phase: 'updating', label: 'Installing', detail: 'The node is downloading the signed release and installing it.' },
  { phase: 'restarting', label: 'Restarting', detail: 'The node is restarting on the new version. It will be offline for a short time.' },
  { phase: 'verifying', label: 'Verifying', detail: 'The node is back online. The hub is confirming it runs the new version.' }
];

const failedStates = ['failed', 'cancelled', 'recovery-required'];

/** Works out how far an update has got from what the hub can observe about the job and the node. */
export function phaseOf(track: NodeUpdateTrack, server: { connectionState: string; version: string }, now: number): UpdatePhase {
  const connected = server.connectionState === 'connected';
  if (connected && server.version.replace(/^v/, '') === track.release) return 'done';
  if (failedStates.includes(track.jobState)) return 'failed';
  if (now - track.startedAt > STALL_AFTER_MS) return 'stalled';
  if (!connected) return 'restarting';
  if (track.sawOffline) return 'verifying';
  return track.jobState === 'queued' ? 'queued' : 'updating';
}

export function isActive(phase: UpdatePhase): boolean {
  return phase !== 'done' && phase !== 'failed' && phase !== 'stalled';
}

export function elapsedLabel(ms: number): string {
  const seconds = Math.max(0, Math.round(ms / 1000));
  if (seconds < 60) return `${seconds}s`;
  return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, '0')}s`;
}

/** The text for a failed job, using the hub's message when it gave one. */
export function failureMessage(error: { code?: string; message?: string } | undefined, jobState: string): string {
  if (error?.message) return error.message;
  if (error?.code) return `The update failed (${error.code}).`;
  if (jobState === 'cancelled') return 'The update was cancelled.';
  if (jobState === 'recovery-required') return 'The update needs recovery on the node. Check the node before retrying.';
  return 'The update failed.';
}
