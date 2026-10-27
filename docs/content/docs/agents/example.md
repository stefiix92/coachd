---
title: Example week
weight: 2
---

{{< callout type="info" >}}
**Illustrative.** A made-up runner, data and conversation, to show how the pieces fit. Tool names and arguments are real; results are shortened.
{{< /callout >}}

## Monday: plan Tuesday

> **Runner:** Morning. Slept so-so, legs fine. What about tomorrow?

The agent checks freshness, then reads the context:

```text
get_status {}
  → sync: every data type last_outcome "ok", age_hours < 2; garmin_key.days_left 61; pending_proposals []
get_goal {}
  → name "Autumn half marathon", date "2026-11-29", distance_km 21.1, target "under 1:45"
get_notes {}
  → [injury] "Left calf tight after hills", created_by "runner", valid_until "2026-11-08"
get_daily_health {"from": "2026-10-26", "to": "2026-11-02"}
get_summaries {"from": "2026-10-05", "to": "2026-11-02"}
```

They talk it through and settle on threshold intervals on a flat route, because of the calf note. The agent proposes:

```json
propose_workout {
  "workout": {
    "name": "Threshold 3 x 8 min",
    "sport": "running",
    "date": "2026-11-03",
    "notes": "Flat route.",
    "steps": [
      {"kind": "warmup", "duration": {"type": "time", "time_s": 900}},
      {"kind": "repeat", "count": 3, "steps": [
        {"kind": "active", "duration": {"type": "time", "time_s": 480},
         "target": {"type": "pace", "low_pace_s_per_km": 285, "high_pace_s_per_km": 295}},
        {"kind": "recovery", "duration": {"type": "time", "time_s": 120}}
      ]},
      {"kind": "cooldown", "duration": {"type": "time", "time_s": 600}}
    ]
  },
  "rationale": "Threshold work for the half. Flat route: calf note until 8 Nov."
}
  → verdict "pending", id "3f9c…", warnings []
```

## The runner approves

ntfy on the phone: "Workout for Tue 3 Nov ready to review". The page shows the steps and the agent's reason, labelled as agent text. **Approve with passkey**: the status is `approved`. No tool could have done this step.

## Tuesday: run it

The runner opens coachd on the watch. It syncs, shows the workout as next, and confirms it: `delivered`. START from the menu runs it in the watch's workout player.

## Tuesday evening: planned vs actual

> **Runner:** Done. Felt good, calf quiet.

```text
request_sync {}
  → outcome "synced"
list_workouts {"from": "2026-11-03", "to": "2026-11-03"}
  → status "delivered", activity_ids [24781234567]
get_activity {"id": 24781234567, "detail": "laps"}
  → perceived {feel_pct 75, rpe 7}
    workout.proposal_id "3f9c…"
    workout.executed: step_index 1 (active), iteration 1: duration_s 480, avg_pace_s_per_km 288,
      target_time {measured_s 480, in_target_s 402, below_target_s 61, above_target_s 17}
    … the same for iterations 2 and 3
```

The agent compares each interval with its pace range, reads the runner's own rating, and they plan the rest of the week the same way. A short note for the watch:

```text
send_message {"text": "Thu: easy 40 min. Calf check before you go."}
```

Next: [starter instructions](/docs/agents/instructions/) to give your agent these habits.
