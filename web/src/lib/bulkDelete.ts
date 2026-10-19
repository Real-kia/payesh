/** Bound concurrent requests and retain partial results if one deletion fails. */
export async function deleteServersConcurrently<T>(
  targets: readonly T[],
  remove: (target: T) => Promise<void>,
  options: { onDeleted?: (target: T) => void; isAuthFailure?: (error: unknown) => boolean } = {}
): Promise<{ deleted: T[]; errors: unknown[] }> {
  const deleted: T[] = [];
  const errors: unknown[] = [];
  let next = 0;
  let stopped = false;
  await Promise.all(Array.from({ length: Math.min(4, targets.length) }, async () => {
    while (!stopped && next < targets.length) {
      const target = targets[next++];
      try {
        await remove(target);
        deleted.push(target);
        options.onDeleted?.(target);
      } catch (error) {
        errors.push(error);
        if (options.isAuthFailure?.(error)) stopped = true;
      }
    }
  }));
  return { deleted, errors };
}
