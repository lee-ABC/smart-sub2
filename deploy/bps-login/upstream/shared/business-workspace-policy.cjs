'use strict';

// Account names and organization structure never establish subscription status.
const BUSINESS_PLANS = new Set(['team', 'business', 'chatgptteamplan', 'chatgptbusinessplan', 'self_serve_business', 'self_serve_business_prolite']);
const PERSONAL_PLANS = new Set(['free', 'plus', 'pro', 'go', 'chatgptfreeplan', 'chatgptplusplan', 'chatgptproplan', 'chatgptgoplan']);
const OTHER_PLANS = new Set(['enterprise', 'edu', 'education', 'chatgptenterpriseplan', 'chatgpteduplan', 'self_serve_enterprise']);
const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const text = (value, max = 256) => typeof value === 'string' && value.trim() === value && value.length > 0 && value.length <= max && !/[\x00-\x1f\x7f]/.test(value);
const normalizedPlan = value => typeof value === 'string' ? value.trim().toLowerCase() : '';
const planKind = value => BUSINESS_PLANS.has(value) ? 'business' : PERSONAL_PLANS.has(value) ? 'personal' : OTHER_PLANS.has(value) ? 'other' : 'unknown';
const isBusinessPlan = value => BUSINESS_PLANS.has(normalizedPlan(value));
const identifier = value => text(value) && !['__proto__', 'prototype', 'constructor'].includes(value);

function invalid() {
  const error = new Error('工作空间订阅信息不完整，请稍后重试');
  error.code = 'INVALID_LOGIN_RESPONSE';
  error.retryable = true;
  throw error;
}

function timeNow(now) {
  const value = typeof now === 'function' ? now() : now ?? Date.now();
  if (!Number.isFinite(value)) invalid();
  return value;
}

function expiry(value) {
  if (value === null) return null;
  if (typeof value !== 'string' || value.length > 64 || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(value)) invalid();
  const stamp = Date.parse(value);
  const [year, month, day] = value.slice(0, 10).split('-').map(Number);
  const date = new Date(Date.UTC(year, month - 1, day));
  if (!Number.isFinite(stamp) || date.getUTCFullYear() !== year || date.getUTCMonth() !== month - 1 || date.getUTCDate() !== day) invalid();
  return new Date(stamp).toISOString();
}

function flags(entry, account, fieldNames, required = false) {
  const values = [];
  for (const source of [entry, account]) for (const name of fieldNames) if (Object.hasOwn(source, name)) {
    if (typeof source[name] !== 'boolean') invalid();
    values.push(source[name]);
  }
  if (required && !values.length) invalid();
  return values.some(Boolean);
}

function snapshotComplete(raw, count) {
  let complete = true;
  const sources = [raw];
  for (const key of ['pagination', 'paging']) if (Object.hasOwn(raw, key)) {
    if (!object(raw[key])) invalid();
    sources.push(raw[key]);
  }
  for (const source of sources) {
    for (const key of ['has_more', 'hasMore']) if (Object.hasOwn(source, key)) {
      if (typeof source[key] !== 'boolean') invalid();
      if (source[key]) complete = false;
    }
    for (const key of ['next_cursor', 'nextCursor', 'next_page', 'nextPage']) if (Object.hasOwn(source, key)) {
      const value = source[key];
      if (value === null || value === '' || value === false || value === 0) continue;
      if (typeof value === 'string' && text(value, 4096) || Number.isSafeInteger(value) && value > 0 || value === true) complete = false;
      else invalid();
    }
    for (const key of ['total_accounts', 'totalAccounts']) if (Object.hasOwn(source, key)) {
      if (!Number.isSafeInteger(source[key]) || source[key] < 0) invalid();
      if (source[key] > count) complete = false;
    }
  }
  return complete;
}

function identityMap(workspaces) {
  const identities = new Map();
  const claim = (alias, item) => {
    if (!identifier(alias) || identities.has(alias) && identities.get(alias).id !== item.id) invalid();
    identities.set(alias, item);
  };
  for (const item of workspaces) {
    if (!object(item) || !identifier(item.id)) invalid();
    claim(item.id, item);
  }
  for (const item of workspaces) {
    if (item.aliases === undefined && !Object.hasOwn(item, 'aliases')) continue;
    if (!Array.isArray(item.aliases) || item.aliases.length < 1 || item.aliases.length > 101 || new Set(item.aliases).size !== item.aliases.length) invalid();
    for (const alias of item.aliases) claim(alias, item);
  }
  return identities;
}

function parseWorkspaceCheckResponse(raw, { now } = {}) {
  const checkedAt = timeNow(now);
  if (!object(raw) || !object(raw.accounts) || Object.keys(raw.accounts).length < 1 || Object.keys(raw.accounts).length > 100) invalid();
  const byId = new Map();
  const aliases = new Map();
  for (const [alias, entry] of Object.entries(raw.accounts)) {
    if (!identifier(alias) || !object(entry) || !object(entry.account) || !object(entry.entitlement)) invalid();
    const account = entry.account;
    const entitlement = entry.entitlement;
    if (!identifier(account.account_id) || !text(account.plan_type, 100)) invalid();
    if (!Object.hasOwn(entitlement, 'expires_at') || typeof entitlement.has_active_subscription !== 'boolean') invalid();
    if (entitlement.subscription_plan !== null && !text(entitlement.subscription_plan, 100)) invalid();
    const accountPlan = normalizedPlan(account.plan_type);
    const subscriptionPlan = normalizedPlan(entitlement.subscription_plan);
    const accountKind = planKind(accountPlan);
    const subscriptionKind = subscriptionPlan ? planKind(subscriptionPlan) : accountKind;
    if (accountKind === 'unknown' || subscriptionKind === 'unknown' || accountKind !== subscriptionKind) invalid();
    const expiresAt = expiry(entitlement.expires_at);
    const deactivated = flags(entry, account, ['is_deactivated', 'is_disabled'], true);
    const viewOnly = flags(entry, account, ['is_view_only', 'is_read_only'], true);
    const name = account.name ?? account.display_name ?? '';
    if (typeof name !== 'string' || name.length > 256 || /[\x00-\x1f\x7f]/.test(name)) invalid();
    const item = {
      id: account.account_id, name, kind: accountKind, plan: accountPlan,
      active: entitlement.has_active_subscription && !deactivated && !viewOnly && (expiresAt === null || Date.parse(expiresAt) > checkedAt),
      expiresAt, deactivated, viewOnly, aliases: [account.account_id, ...(alias === account.account_id ? [] : [alias])],
    };
    const previous = byId.get(item.id);
    if (previous && ['kind', 'plan', 'active', 'expiresAt', 'deactivated', 'viewOnly'].some(key => previous[key] !== item[key])) invalid();
    if (previous) item.aliases = [...new Set([...previous.aliases, ...item.aliases])];
    if (!previous || alias === item.id) byId.set(item.id, item);
    else previous.aliases = item.aliases;
    aliases.set(alias, item.id);
  }
  // An account-map key must never impersonate another entry's canonical ID.
  identityMap([...byId.values()]);
  if (raw.account_ordering !== undefined && !Array.isArray(raw.account_ordering)) invalid();
  const order = [];
  for (const entry of raw.account_ordering || []) {
    if (!text(entry)) invalid();
    const id = aliases.get(entry) || entry;
    if (!byId.has(id)) invalid();
    if (!order.includes(id)) order.push(id);
  }
  return { workspaces: [...byId.values()], order, complete: snapshotComplete(raw, byId.size) };
}

function chooseBusinessWorkspace(inspection, { currentWorkspaceId, now } = {}) {
  const checkedAt = timeNow(now);
  if (!object(inspection) || !Array.isArray(inspection.workspaces) || inspection.workspaces.length < 1 || inspection.workspaces.length > 100 || !Array.isArray(inspection.order) || inspection.order.length > 100) invalid();
  if (Object.hasOwn(inspection, 'complete') && typeof inspection.complete !== 'boolean') invalid();
  const identities = identityMap(inspection.workspaces);
  const candidates = new Map();
  const seenIds = new Set();
  for (const item of inspection.workspaces) {
    if (!object(item) || !text(item.id) || !['business', 'personal', 'other'].includes(item.kind) || !text(item.plan, 100) || planKind(normalizedPlan(item.plan)) !== item.kind || typeof item.active !== 'boolean' || typeof item.deactivated !== 'boolean' || typeof item.viewOnly !== 'boolean') invalid();
    const expiresAt = expiry(item.expiresAt);
    if (seenIds.has(item.id)) invalid();
    seenIds.add(item.id);
    if (item.kind === 'business' && isBusinessPlan(item.plan) && item.active && !item.deactivated && !item.viewOnly && (expiresAt === null || Date.parse(expiresAt) > checkedAt)) candidates.set(item.id, item);
  }
  for (const id of inspection.order) {
    if (!identifier(id) || !identities.has(id)) invalid();
  }
  const current = identities.get(currentWorkspaceId);
  if (current && candidates.has(current.id)) return candidates.get(current.id);
  for (const id of inspection.order) {
    const item = identities.get(id);
    if (candidates.has(item.id)) return candidates.get(item.id);
  }
  return [...candidates.values()].sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0)[0] || null;
}

module.exports = { parseWorkspaceCheckResponse, chooseBusinessWorkspace, isBusinessPlan };
