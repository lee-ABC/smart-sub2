'use strict';

const closePromises = new WeakMap();
const MAX_CLOSE_MS = 2000;
const MAX_OPERATION_MS = 30000;

function boundedMilliseconds(value, maximum) {
    if (!Number.isFinite(value) || value <= 0) throw new TypeError('Invalid operation deadline');
    return Math.min(value, maximum);
}

/** Repeated cleanup shares one close attempt, including a hanging native close. */
function closeBounded(resource, { timeoutMs = MAX_CLOSE_MS } = {}) {
    if (!resource || (typeof resource !== 'object' && typeof resource !== 'function') || typeof resource.close !== 'function') return Promise.resolve();
    if (closePromises.has(resource)) return closePromises.get(resource);
    const duration = boundedMilliseconds(timeoutMs, MAX_CLOSE_MS);
    const pending = Promise.resolve().then(() => resource.close()).catch(() => {});
    let timer;
    const deadline = new Promise(resolve => { timer = setTimeout(resolve, duration); });
    const result = Promise.race([pending, deadline]).finally(() => clearTimeout(timer));
    closePromises.set(resource, result);
    return result;
}

/** Races native promises without leaving rejection handlers or abort listeners behind. */
function createOperationDeadline({ signal, timeoutMs = MAX_OPERATION_MS, makeError } = {}) {
    const duration = boundedMilliseconds(timeoutMs, MAX_OPERATION_MS);
    const controller = new AbortController();
    const cancel = () => { if (!controller.signal.aborted) controller.abort(makeError('CANCELLED')); };
    const timer = setTimeout(() => {
        if (!controller.signal.aborted) controller.abort(makeError('UPSTREAM_UNAVAILABLE'));
    }, duration);
    signal?.addEventListener('abort', cancel, { once: true });
    if (signal?.aborted) cancel();

    function run(operation, { onLateResolve } = {}) {
        if (controller.signal.aborted) return Promise.reject(controller.signal.reason);
        return new Promise((resolve, reject) => {
            let settled = false;
            const finish = (callback, value) => {
                if (settled) return;
                settled = true;
                controller.signal.removeEventListener('abort', abort);
                callback(value);
            };
            const abort = () => finish(reject, controller.signal.reason);
            controller.signal.addEventListener('abort', abort, { once: true });
            if (controller.signal.aborted) return abort();
            Promise.resolve().then(() => {
                if (controller.signal.aborted) throw controller.signal.reason;
                return operation();
            }).then(value => {
                if (!settled) finish(resolve, value);
                else if (onLateResolve) Promise.resolve().then(() => onLateResolve(value)).catch(() => {});
            }, error => finish(reject, error));
        });
    }

    return {
        signal: controller.signal,
        run,
        cleanup() {
            clearTimeout(timer);
            signal?.removeEventListener('abort', cancel);
        },
    };
}

module.exports = { closeBounded, createOperationDeadline, MAX_CLOSE_MS, MAX_OPERATION_MS };
