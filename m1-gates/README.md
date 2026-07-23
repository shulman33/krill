# m1-gates

Runnable acceptance gates for milestone M1 (`krilld`), pre-registered in
`../ROADMAP.md` **before** the daemon was built:

| Gate | Claim | Pass condition |
|---|---|---|
| A1 | wake-on-request is fast | curl of a FROZEN app returns 200; warm-wake p99 ≤ 300 ms over 100 wakes, end-to-end through the router |
| A2 | scale-to-zero is unattended | idle timeout demotes ACTIVE → FROZEN with no operator action; the VMM process (and its RAM) is gone; the app wakes again |
| A3 | sleep is safe for data | 100 sleep/wake cycles of an app doing acked SQLite writes: integrity_check ok, seq ledger gapless |
| A4 | fleets fit on one host | 10 apps resident as snapshots simultaneously; every one wakes with its own identity (kernel-assigned IP) and its own state |

## Run

On a nested-virt dev box provisioned per `../wake-bench/README.md` (with
`wake-bench/00-host-setup.sh` and `01-install-firecracker.sh` already run):

```bash
# locally:
make krilld-linux && scp bin/krilld-linux-amd64 root@<box>:~/m1-gates/krilld
scp -r m1-gates root@<box>:~

# on the box, as root:
cd m1-gates
./00-setup.sh          # build gate guest image, start krilld (idle-timeout 15s)
./a1-warm-wake.sh      # ~15 min (100 freeze/wake cycles)
./a2-idle-freeze.sh    # ~1 min (waits out the idle timeout, hands off)
./a3-sqlite-cycles.sh  # ~15 min (100 write/freeze/wake cycles)
./a4-ten-apps.sh       # ~5 min
```

Each script prints `== GATE An: PASS/FAIL == ` with the measured numbers and
leaves raw data in `results/`. Fill `RESULTS-TEMPLATE.md` and commit the
dated copy, same ritual as wake-bench.

The tier rule applies unchanged: nested-virt PASSes on latency gates are
conclusive (metal is faster); a FAIL escalates to metal before it may be
recorded as a FAIL.

## Notes

- Timing is host-side only; guest clocks jump across resume.
- The guest follows the M1 network contract: krilld appends
  `krill_ip=/krill_gw=` to the kernel cmdline, `/init.sh` applies them.
- `gate-a1`, `gate-a2`, `gate-a3`, `gate-fleet-*` are independent apps; a
  wedged run can `curl -X DELETE $ADMIN/v1/apps/<name>` and rerun.
