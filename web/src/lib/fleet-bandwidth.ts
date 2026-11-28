import type { PreviewServer } from '../preview/fixtures';

const maxSampleAge = 90;

/** Aggregate the hub's recent per-server rates on their actual sample timestamps. */
export function recentFleetBandwidth(servers: PreviewServer[], recordedThrough: number, windowStart: number) {
  const histories = servers.filter((server) => server.connectionState === 'connected')
    .map((server) => server.metricHistory?.ranges['15m']);
  const times = [...new Set(histories.flatMap((history) => history?.timestamps ?? []))].sort((a, b) => a - b);
  const firstNew = times.findIndex((time) => time > recordedThrough && time >= windowStart);
  const timestamps: number[] = [];
  const networkRx: (number | null)[] = [];
  const networkTx: (number | null)[] = [];
  if (firstNew === -1) return { timestamps, networkRx, networkTx };

  // Include the nearest raw sample at the handoff so the live line can meet the saved history.
  const previous = times[firstNew - 1];
  const includePrevious = previous >= windowStart && recordedThrough - previous <= maxSampleAge;
  const positions = histories.map(() => -1);
  for (const timestamp of times.slice(includePrevious ? firstNew - 1 : firstNew)) {
    const last = timestamps.at(-1);
    if (last !== undefined && timestamp - last > maxSampleAge) {
      timestamps.push(last + maxSampleAge);
      networkRx.push(null);
      networkTx.push(null);
    }

    let download: number | null = 0;
    let upload: number | null = 0;
    histories.forEach((history, index) => {
      let position = positions[index];
      while (history && history.timestamps[position + 1] <= timestamp) position++;
      positions[index] = position;
      const current = history && position >= 0 && timestamp - history.timestamps[position] <= maxSampleAge;
      const rx = current ? history.networkRx?.[position] : null;
      const tx = current ? history.networkTx?.[position] : null;
      download = download !== null && rx != null ? download + rx : null;
      upload = upload !== null && tx != null ? upload + tx : null;
    });
    timestamps.push(timestamp);
    networkRx.push(download);
    networkTx.push(upload);
  }
  return { timestamps, networkRx, networkTx };
}
