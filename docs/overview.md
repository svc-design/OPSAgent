# Project Overview

This document describes the initial design for OPSAgent based on the
"OPS Agent 思维链条". The agent follows a five step workflow to diagnose
issues:

1. **Who is in trouble** – Identify services with abnormal metrics.
2. **When** – Determine the time period where the issue occurred.
3. **Which request** – Collect traces and logs for the affected service.
4. **Where** – Locate the pods or nodes associated with the failing spans.
5. **What** – Infer the root cause and suggest actions.

The current codebase only provides placeholders for these steps. Future
work will implement real data collectors (Prometheus, Loki, Jaeger/Tempo),
rule engines and reporting sinks such as Slack or Grafana.
