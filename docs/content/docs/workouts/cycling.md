---
title: Cycling and Zwift
weight: 1
---

## Power targets

A cycling step can target power in **% of FTP** or in **watts** (1 to 1500 W), never both in one step. A % of FTP target travels as is, and the watch or Zwift applies its own FTP when you ride. With [intervals.icu](/docs/install/intervals/) connected, an agent can read the FTP set there with `get_profile`, to work out watts; coachd does not use it in a workout.

## On a smart trainer

The `indoor_cycling` sport starts on the watch as an indoor ride, so a watch paired with a smart trainer can drive it. Pair the trainer as a sensor on the watch, then start the workout from the coachd app. If the watch opens an outdoor ride, or the trainer doesn't follow the power, [open an issue](https://github.com/stefiix92/coachd/issues).

## Zwift workouts

A cycling workout whose steps all have a time duration downloads as a Zwift `.zwo` file. Power in % of FTP becomes Zwift power; heart-rate, cadence and effort targets become free-ride blocks with an on-screen message.

1. On the approved workout's page, click **Download for Zwift**.
2. Copy the file into `Documents/Zwift/Workouts/<your Zwift ID>/` on your Mac or PC. If the numbered folder is missing, start Zwift there and sign in once.
3. Restart Zwift: the workout is under **Custom Workouts**.

Zwift on Apple TV or iPad can't import files: import on a Mac or PC, and Zwift's sync brings it over.

A workout with distance, open or heart-rate-bounded steps, or with watt targets, can't be a Zwift workout (`.zwo` power is a fraction of FTP). The page names the step, and the agent gets the same fact as a warning when it proposes it.
