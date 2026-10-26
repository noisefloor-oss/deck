export function selectShard(entries, shard, weights) {
  if (shard === undefined) return entries;
  const match = /^([1-9]\d*)\/([1-9]\d*)$/.exec(shard);
  if (!match) throw new Error("Probe shard must be INDEX/COUNT (one-based)");
  const index = Number(match[1]);
  const count = Number(match[2]);
  if (!Number.isSafeInteger(index) || !Number.isSafeInteger(count)
      || index > count || count > entries.length) {
    throw new Error("Probe shard must select a nonempty subset of the inventory");
  }
  if (!weights) return entries.filter((_, position) => position % count === index - 1);
  const buckets = Array.from({ length: count }, () => ({ seconds: 0, entries: [] }));
  const names = new Set();
  const ranked = entries.map((entry, position) => {
    const name = Array.isArray(entry) ? entry[0] : entry;
    if (typeof name !== "string" || names.has(name)) throw new Error("Probe names must be unique strings");
    names.add(name);
    const seconds = Object.hasOwn(weights, name) ? weights[name] : 10;
    if (!Number.isFinite(seconds) || seconds < 0) throw new Error("Probe duration must be nonnegative and finite");
    return { entry, position, seconds: Math.max(seconds, 0.001) };
  }).sort((a, b) => b.seconds - a.seconds || a.position - b.position);
  for (const item of ranked) {
    const bucket = buckets.reduce((best, candidate) => candidate.seconds < best.seconds ? candidate : best);
    bucket.entries.push(item);
    bucket.seconds += item.seconds;
  }
  return buckets[index - 1].entries.sort((a, b) => a.position - b.position).map(({ entry }) => entry);
}
