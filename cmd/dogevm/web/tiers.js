// How many Dogecoin confirmations a deposit needs before it's credited.
// Smaller deposits need fewer (the bridge's confirmation tiers); larger ones
// need the full count. Shared by the wallet and the docs.

const koinu = (doge) => {
  const [whole, frac = ''] = String(doge).split('.');
  return BigInt(whole) * 100000000n + BigInt((frac + '00000000').slice(0, 8));
};
const plain = (doge) => String(doge).replace(/\.?0+$/, '');
const plural = (n) => `${n} confirmation${n === 1 ? '' : 's'}`;

// confirmationsFor is the count for a deposit of value koinu (a BigInt).
export function confirmationsFor(info, value) {
  for (const t of info.confirmationTiers || []) {
    if (value <= koinu(t.upTo)) return t.confirmations;
  }
  return info.depositConfirmations;
}

// describeTiers says how long deposits wait, for example "1 confirmation
// for up to 1 DOGE, 6 for up to 10 DOGE, 12 for up to 50 DOGE, and 20 for
// anything larger".
export function describeTiers(info) {
  const tiers = info.confirmationTiers || [];
  if (tiers.length === 0) return plural(info.depositConfirmations);
  const parts = tiers.map((t, i) => `${i === 0 ? plural(t.confirmations) : t.confirmations} for up to ${plain(t.upTo)} DOGE`);
  return `${parts.join(', ')}, and ${info.depositConfirmations} for anything larger`;
}
