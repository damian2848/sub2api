export class PrismError extends Error {
  constructor(code, status = 502, param) {
    super(code);
    this.code = code;
    this.status = status;
    this.param = param;
  }
}

export function aborted(signal) {
  if (signal?.aborted) throw signal.reason instanceof PrismError ? signal.reason : new PrismError('request_cancelled', 499);
}

export async function interruptible(work, signal, cancel) {
  aborted(signal);
  let listener;
  const cancellation = new Promise((_, reject) => {
    listener = () => reject(signal.reason instanceof PrismError ? signal.reason : new PrismError('request_cancelled', 499));
    signal?.addEventListener('abort', listener, { once: true });
  });
  try {
    return await Promise.race([work(), cancellation]);
  } catch (error) {
    if (signal?.aborted) await cancel?.();
    throw error;
  } finally {
    signal?.removeEventListener('abort', listener);
  }
}

export function publicError(error) {
  const safe = error instanceof PrismError ? error : new PrismError('browser_operation_failed');
  return { error: { message: safe.code, type: safe.status < 500 ? 'invalid_request_error' :
    'prism_error', code: safe.code, ...(safe.param ? { param: safe.param } : {}) } };
}
