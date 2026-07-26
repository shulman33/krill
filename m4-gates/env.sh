# Shared configuration for the M4 gates. Sourced by every script.
#
# Unlike M1-M3, these run against the PRODUCTION daemon on krill-fsn1: M4 is
# the first milestone whose prerequisites a bench VM cannot supply — a real
# domain, a real certificate, a real Google OAuth client. Nothing here
# restarts krilld (it is under Restart=always; see "Running the gate suites on
# this box" in SERVER-SETUP.md), and nothing here writes to a scratch data dir
# either: F4 must run against the box that will still be serving next week.

ADMIN=${ADMIN:-http://127.0.0.1:9091}          # krilld
DOORMAN=${DOORMAN:-http://127.0.0.1:9092}      # krill-doorman control API
DOORMAN_PUB=${DOORMAN_PUB:-http://127.0.0.1:9090}  # what Caddy proxies to
BASE_HOST=${BASE_HOST:-krill.run}
AUTH_HOST=${AUTH_HOST:-auth.$BASE_HOST}
SCHEME=${SCHEME:-https}

# The apps the gates use. ledger is M3's data-plane example (it writes
# /data/app.db and therefore has a real stream); watchlist is F4's.
APP=${APP:-watchlist}
OTHER_APP=${OTHER_APP:-ledger}

DOORMAN_STATE=${DOORMAN_STATE:-/var/lib/krill-doorman}
DOORMAN_UNIT=${DOORMAN_UNIT:-krill-doorman}

RESULTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/results"
mkdir -p "$RESULTS_DIR"

# F1 saves the browser session here so F2 and F3 can reuse it. It is a real
# credential for a real account — it lives under results/ which is gitignored,
# and it should be discarded when the run is over.
SESSION_FILE=${SESSION_FILE:-$RESULTS_DIR/session.txt}
SESSION2_FILE=${SESSION2_FILE:-$RESULTS_DIR/session2.txt}
