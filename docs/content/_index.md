---
title: coachd
layout: hextra-home
---

{{< hextra/hero-badge link="https://github.com/stefiix92/coachd" >}}
  <div class="hx:w-2 hx:h-2 hx:rounded-full hx:bg-primary-400"></div>
  <span>Free and open source, self-hosted</span>
  {{< icon name="arrow-circle-right" attributes="height=14" >}}
{{< /hextra/hero-badge >}}

<div class="hx:mt-6 hx:mb-6">
{{< hextra/hero-headline >}}
  A gateway between your AI coach&nbsp;<br class="hx:sm:block hx:hidden" />and your Garmin watch
{{< /hextra/hero-headline >}}
</div>

<div class="hx:mb-12">
{{< hextra/hero-subtitle >}}
  coachd serves your Garmin data to any AI agent over MCP&nbsp;<br class="hx:sm:block hx:hidden" />and puts the workout you agreed on onto your watch, but only after you approve it.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx:mb-6">
{{< hextra/hero-button text="Get started" link="docs" >}}
</div>

<div class="hx:mt-6"></div>

<div style="display:flex;justify-content:center">
  <img src="images/coachd-demo.gif" style="width:100%;max-width:960px;height:auto;border-radius:12px" alt="Animated walkthrough in five steps: Garmin data flows into coachd; an AI agent reads it over MCP and proposes a threshold workout; the runner approves it with a passkey on coachd's page; the watch app syncs it and confirms delivery; after the run, planned versus actual per interval goes back to the agent">
</div>
<p style="text-align:center;font-size:0.875rem;opacity:0.7;margin-top:1rem">How it works in 46 seconds. Mocked, with a made-up runner (<a href="docs/agents/example/">an example week</a>).</p>

<div class="hx:mt-12"></div>

{{< hextra/feature-grid >}}
  {{< hextra/feature-card
    title="Your Garmin data, for any agent"
    icon="chart-bar"
    subtitle="Activities with their FIT files, sleep, HRV and more, from the official Garmin Connect Personal API, over MCP to Claude, ChatGPT or any MCP client."
    link="docs/how-it-works/"
  >}}
  {{< hextra/feature-card
    title="Nothing reaches the watch without you"
    icon="finger-print"
    subtitle="The agent proposes; you read the workout, its reasons and coachd's warnings, and approve with a passkey. No agent credential can approve, deliver or withdraw."
    link="docs/workouts/"
  >}}
  {{< hextra/feature-card
    title="On your wrist or your trainer"
    icon="lightning-bolt"
    subtitle="The coachd app from the Connect IQ Store copies approved workouts onto your fēnix or Forerunner. Or download the FIT file, or a Zwift workout for cycling."
    link="https://apps.garmin.com/apps/14f05551-a935-44d7-9227-6b4762e4b341"
  >}}
  {{< hextra/feature-card
    title="Planned vs actual"
    icon="clipboard-check"
    subtitle="Executed steps are read back from each activity and linked to the workout delivered, so the agent can review the plan against what you did."
    link="docs/how-it-works/#planned-vs-actual"
  >}}
  {{< hextra/feature-card
    title="Self-hosted, one runner"
    icon="server"
    subtitle="One signed Docker image, one instance per runner. Your Garmin key and your health data stay on your server."
    link="docs/install/"
  >}}
  {{< hextra/feature-card
    title="A gateway, not a coach"
    icon="shield-check"
    subtitle="Numbers, never verdicts. You and your agent decide the training; coachd provides the data, keeps the context, warns and delivers."
    link="docs/"
  >}}
{{< /hextra/feature-grid >}}

<div class="hx:mt-16"></div>

<div style="display:flex;flex-wrap:wrap;justify-content:center;align-items:flex-start;gap:1.5rem">
  <img src="images/approve.png" style="width:250px;height:auto" alt="Approval page on a phone: an agent-proposed threshold workout with its steps, the agent's reason, a warning, and the Approve with passkey button">
  <img src="images/consent.png" style="width:250px;height:auto" alt="Connect an app page: Claude asks for access, showing its web address, where the browser returns and what the app may do">
  <img src="images/apps.png" style="width:250px;height:auto" alt="Connected apps page on a phone: each app with its access, when it connected and a Revoke button">
</div>
<p style="text-align:center;font-size:0.875rem;opacity:0.7;margin-top:1rem">Approving a workout, connecting an app and the connected apps, on a phone. Fake data from a test instance.</p>
