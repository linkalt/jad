// Replaces values that legitimately differ per process (ports, temp paths,
// generated ids, timestamps) so only behavioural differences remain.
const ISO = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z/g;

export function normalize(value, {root, sessions, port, providerUrl}) {
  const providerPort = new URL(providerUrl).port;
  const text = (s) =>
    s.replaceAll(sessions, "$SESSIONS").replaceAll(root, "$ROOT")
      .replaceAll(`127.0.0.1:${port}`, "$SERVER").replaceAll(`127.0.0.1:${providerPort}`, "$PROVIDER")
      .replace(ISO, "$TIME")
      .replace(/Command completed in \d+ ms/g, "Command completed in $N ms")
      // Operating-system error wording differs by runtime (Node reports
      // "ENOENT: no such file or directory, open '…'", Go "open …: no such file
      // or directory") and cannot be matched, so only the prefix is compared.
      .replace(/(could not read MCP server config): .*/, "$1: $OSERROR")
      .replace(/(env_file: could not read \S+): .*/, "$1: $OSERROR")
      // Recoverable front-compaction archives: timestamp plus random suffix.
      .replace(/messages-\d{8}T\d{9}Z-[0-9a-f]{8}\.jsonl/g, "messages-$STAMP.jsonl")
      // Edit backups are named by wall-clock milliseconds plus a random suffix.
      .replace(/\.bak-\d{10,}-[0-9a-f]{16}/g, ".bak-$STAMP")
      // Memory refs embed the creation time; managed processes report their OS pid.
      .replace(/-\d{8}T\d{6}\.\d{3}Z-/g, "-$STAMP-")
      .replace(/\bpid: \d+/g, "pid: $PID")
      .replace(/"pid":\d+/g, '"pid":$PID')
      // Native tools wrap their output in a random per-call content token.
      .replace(/token=r_[A-Za-z0-9_-]{22}/g, "token=$TOKEN")
      // The web UI renders message times as a local wall-clock string.
      .replace(/^\d{1,2}:\d{2}\s?[AP]M$/, "$CLOCK")
      .replace(/"?(run|turn|batch)[_-][0-9a-f]{8}-[0-9a-f-]{27}"?/gi, (m) => (m.startsWith('"') ? '"$ID"' : "$ID"))
      .replace(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi, "$UUID")
      .replace(/"etag":\s*"[^"]*"/g, '"etag":"$ETAG"')
      // Transcript generation is "device:inode:birth-ms"; the inode and birth
      // time differ between two server runs, so only the shape is compared.
      .replace(/^\d+:\d+:\d+$/, "$GENERATION");
  const walk = (v, key) => {
    if (typeof v === "string") return text(v);
    if (Array.isArray(v)) return v.map((x) => walk(x, key));
    if (v && typeof v === "object") {
      return Object.fromEntries(Object.entries(v).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)).map(([k, x]) => [k, VOLATILE.has(k) ? "$VOLATILE" : walk(x, k)]));
    }
    return v;
  };
  return walk(value);
}

// Keys whose values are timestamps, hashes of volatile content or durations.
const VOLATILE = new Set(["__agent_session_input_batch_id", "created_at", "updated_at", "started_at", "completed_at", "finished_at", "duration_ms", "elapsed_ms", "mtime_ms", "modified_at", "etag", "cursor"]);
