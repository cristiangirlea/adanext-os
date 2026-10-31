import { useEffect, useState } from "react";
import type { AdaClient, Experience } from "../client/api";
import { Marked } from "../text/Marked";

export function ExperienceCard({ client }: { client: AdaClient }) {
  const [value, setValue] = useState<Experience | "error" | null>(null);
  useEffect(() => {
    let live = true;
    client.experience().then((v) => { if (live) setValue(v); }, () => { if (live) setValue("error"); });
    return () => { live = false; };
  }, [client]);
  if (value === "error") return <p className="empty">Experience is unavailable.</p>;
  if (!value || !value.enabled) return null;
  return <section className="note" aria-label="Experience">
    <strong>Level {value.level} &middot; {value.xp} XP</strong>
    <p>Delivery {value.week.delivery} &middot; Practice {value.week.practice} this week</p>
    <p>Streak: {value.streak ?? "-"}</p>
    <div role="img" aria-label={`35-day streak, oldest first: ${value.days.map((day) => day ? "active" : "inactive").join(", ")}`}>
      {value.days.map((day, i) => <span key={i} aria-hidden="true">{day ? "\u25cf" : "\u25cb"}</span>)}
    </div>
    {value.badges.length > 0 && <p>Badges: {value.badges.map((badge, i) => <span key={i}>{i > 0 && ", "}<Marked text={badge} /></span>)}</p>}
    {value.exercise && <p>Today's exercise: <Marked text={value.exercise.kind} /> &middot; {value.exercise.answered ? "answered" : <Marked text={value.exercise.material ?? ""} />}</p>}
  </section>;
}
