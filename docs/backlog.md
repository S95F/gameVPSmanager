# Backlog — leftovers from archived docs

When a finished planning or review document moves to [`docs/completed/`](completed/),
any item still pending in it lands here as an incomplete item, attributed to the
document it came from. Nothing is closed by being archived; it is closed by being
done, or by an explicit decision recorded where the item is listed.

---

## From [`completed/IMPLEMENTATION-PLAN.md`](completed/IMPLEMENTATION-PLAN.md) (archived 2026-10-17)

Phases 0–19 are committed with `make check` green; the per-phase definition-of-done
checkboxes were never ticked as the phases landed and do not represent pending work.
Foundation surface that exists but is not yet wired is tracked by
[`USABILITY-PLAN.md`](USABILITY-PLAN.md) (Phases 20–55) and the findings in
[`reviews/usability-2026-10.md`](reviews/usability-2026-10.md), not duplicated here.

Still genuinely pending is the plan's closing checklist, **Definition of done for v1
as a whole** — end-state goals no single phase owns:

- [ ] Milestones A–D demonstrated, each with a recorded transcript.
- [ ] The live Project Zomboid server has run under gvm for **7 consecutive days** with nightly updates and backups, then `gvm adopt finalize` executed.
- [ ] `gvm doctor` reports **0 problems** on that host.
- [ ] `ci/control-plane-dead.sh` green on every PR for the preceding two weeks.
- [ ] `make check` green on Windows 11 with no build tags required.
- [ ] Every decision in the architecture's register (D-01…D-46) is either implemented or has an open issue explaining why not.
- [ ] Every open question in ARCHITECTURE §23 has a recorded measurement, or an issue with the measurement plan.

## From [`completed/bug-sweep-phases-14-17.md`](completed/bug-sweep-phases-14-17.md) (archived 2026-10-17)

No pending items. Seven findings fixed in `2807117`; BUG-008 and BUG-009 are
recorded there as accepted will-not-fix decisions, not open work.

## From [`completed/bug-sweep-phases-18-19.md`](completed/bug-sweep-phases-18-19.md) (archived 2026-10-17)

No pending items. All nine findings were fixed in the sweep commit.
