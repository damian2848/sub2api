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

// Fixed, human-readable reasons for the failures people actually see. They never carry upstream
// text; any other code is its own message.
const MESSAGES = {
  prism_upstream_http_error: 'Prism returned a server error while generating (its servers may be overloaded); try again shortly',
  prism_generation_failed: 'Prism could not complete the generation; try again',
  prism_empty_output: 'Prism returned an empty answer',
  prism_invalid_output: 'Prism returned an answer that could not be read',
  upstream_model_mismatch: 'Prism answered with a different model than the one requested',
  session_expired: 'The Prism session expired and is reconnecting',
  model_not_available: 'This model is not offered by Prism for this account',
  account_not_ready: 'The Prism account is not ready yet',
  account_busy: 'The Prism account is busy; try again shortly',
  account_queue_full: 'The Prism account is busy; try again shortly',
  request_timeout: 'Prism did not answer in time',
  sandbox_initialization_timeout: 'The Prism sandbox did not become ready in time',
};

export function publicError(error) {
  const safe = error instanceof PrismError ? error : new PrismError('browser_operation_failed');
  return { error: { message: MESSAGES[safe.code] ?? safe.code, type: safe.status < 500 ? 'invalid_request_error' :
    'prism_error', code: safe.code, ...(safe.param ? { param: safe.param } : {}) } };
}
