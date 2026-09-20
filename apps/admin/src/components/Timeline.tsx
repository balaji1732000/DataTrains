export type TrajectoryAction = {
  timestamp_ns: number;
  type: string;
  application?: string;
  x?: number;
  y?: number;
  button?: string;
  key?: string;
  delta_x?: number;
  delta_y?: number;
};

type TimelineProps = {
  actions: TrajectoryAction[];
  currentNS: number;
  durationNS: number;
  onSeek: (timestampNS: number) => void;
};

export function Timeline({ actions, currentNS, durationNS, onSeek }: TimelineProps) {
  const center = lowerBound(actions, currentNS);
  const start = Math.max(0, center - 20);
  const visible = actions.slice(start, start + 50);
  return (
    <section className="timelinePanel" aria-label="Synchronized input timeline">
      <div className="timelineHeading">
        <div>
          <p className="eyebrow">Input timeline</p>
          <h3>{actions.length.toLocaleString()} permitted events</h3>
        </div>
        <time>{formatTimestamp(currentNS)}</time>
      </div>
      <input
        className="scrubber"
        type="range"
        min={0}
        max={Math.max(durationNS, 1)}
        step={10_000_000}
        value={Math.min(currentNS, durationNS)}
        aria-label="Trajectory position"
        onChange={(event) => onSeek(Number(event.target.value))}
      />
      <ol className="eventList" start={start + 1}>
        {visible.map((action, index) => {
          const isCurrent = start + index === Math.max(0, center - 1);
          return (
            <li key={`${action.timestamp_ns}-${start + index}`} className={isCurrent ? "currentEvent" : undefined}>
              <button type="button" onClick={() => onSeek(action.timestamp_ns)}>
                <time>{formatTimestamp(action.timestamp_ns)}</time>
                <strong>{action.type.replaceAll("_", " ")}</strong>
                <span>{describeAction(action)}</span>
              </button>
            </li>
          );
        })}
      </ol>
    </section>
  );
}

function lowerBound(actions: TrajectoryAction[], timestampNS: number) {
  let low = 0;
  let high = actions.length;
  while (low < high) {
    const middle = Math.floor((low + high) / 2);
    if ((actions[middle]?.timestamp_ns ?? 0) <= timestampNS) low = middle + 1;
    else high = middle;
  }
  return low;
}

function describeAction(action: TrajectoryAction) {
  if (typeof action.x === "number" && typeof action.y === "number") return `${action.x}, ${action.y}`;
  if (action.key) return action.key;
  if (action.button) return action.button;
  if (typeof action.delta_y === "number") return `scroll ${action.delta_x ?? 0}, ${action.delta_y}`;
  return action.application ?? "task application";
}

function formatTimestamp(nanoseconds: number) {
  const totalSeconds = nanoseconds / 1_000_000_000;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = (totalSeconds % 60).toFixed(2).padStart(5, "0");
  return `${minutes}:${seconds}`;
}
