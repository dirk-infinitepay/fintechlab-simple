#!/bin/sh
# Generate a local lab CA + receiver/banking-circle/pod-bank-rails server certs + client cert.
# Idempotent: existing files are left alone unless FORCE_CERTS=1.
#
# CERT_EXTRA_SANS adds names to the two server certificates a client outside
# the lab dials (receiver, banking-circle), e.g.
#   CERT_EXTRA_SANS="DNS:lab.example.test,IP:10.0.0.5"
# for a lab running on another machine. It only applies to certificates
# being generated: FORCE_CERTS=1 to reissue existing ones.
set -eu

CERTS="${CERTS_DIR:-/keys/certs}"
EXTRA="${CERT_EXTRA_SANS:+,$CERT_EXTRA_SANS}"
DAYS="${CERT_DAYS:-825}"
mkdir -p "$CERTS"

need() {
  if [ "${FORCE_CERTS:-}" = "1" ]; then
    return 0
  fi
  [ ! -f "$1" ]
}

if ! command -v openssl >/dev/null 2>&1; then
  echo "openssl is required to generate lab certs" >&2
  exit 1
fi

if need "$CERTS/ca.pem" || need "$CERTS/ca-key.pem"; then
  echo "generating lab CA"
  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$CERTS/ca-key.pem" \
    -out "$CERTS/ca.pem" \
    -days "$DAYS" \
    -subj "/CN=fintech-sim-lab CA/O=fintech-sim-lab"
fi

SAN="subjectAltName=DNS:receiver,DNS:localhost,DNS:receiver.local,DNS:receiver.fintechlab-simple.test,IP:127.0.0.1$EXTRA"
if need "$CERTS/receiver.pem" || need "$CERTS/receiver-key.pem"; then
  echo "generating receiver server cert"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$CERTS/receiver-key.pem" \
    -out "$CERTS/receiver.csr" \
    -subj "/CN=receiver/O=fintech-sim-lab"
  openssl x509 -req \
    -in "$CERTS/receiver.csr" \
    -CA "$CERTS/ca.pem" \
    -CAkey "$CERTS/ca-key.pem" \
    -CAcreateserial \
    -out "$CERTS/receiver.pem" \
    -days "$DAYS" \
    -extfile /dev/stdin <<EOF
$SAN
extendedKeyUsage=serverAuth
EOF
  rm -f "$CERTS/receiver.csr"
fi

# Banking Circle mock is also an mTLS server (docs/ARCHITECTURE-vendor-
# corrections.md section 3): it needs its own server cert, distinct from
# receiver's, signed by the same lab CA.
BC_SAN="subjectAltName=DNS:banking-circle,DNS:localhost,DNS:banking-circle.fintechlab-simple.test,IP:127.0.0.1$EXTRA"
if need "$CERTS/banking-circle.pem" || need "$CERTS/banking-circle-key.pem"; then
  echo "generating banking-circle server cert"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$CERTS/banking-circle-key.pem" \
    -out "$CERTS/banking-circle.csr" \
    -subj "/CN=banking-circle/O=fintech-sim-lab"
  openssl x509 -req \
    -in "$CERTS/banking-circle.csr" \
    -CA "$CERTS/ca.pem" \
    -CAkey "$CERTS/ca-key.pem" \
    -CAcreateserial \
    -out "$CERTS/banking-circle.pem" \
    -days "$DAYS" \
    -extfile /dev/stdin <<EOF
$BC_SAN
extendedKeyUsage=serverAuth
EOF
  rm -f "$CERTS/banking-circle.csr"
fi

# pod-bank-rails twin: dedicated leaf so Connect-surface mTLS on :9085
# can run side-by-side with standalone banking-circle without sharing SANs.
POD_BR_SAN="subjectAltName=DNS:pod-bank-rails,DNS:localhost,DNS:pod-bank-rails.fintechlab-simple.test,IP:127.0.0.1"
if need "$CERTS/pod-bank-rails.pem" || need "$CERTS/pod-bank-rails-key.pem"; then
  echo "generating pod-bank-rails server cert"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$CERTS/pod-bank-rails-key.pem" \
    -out "$CERTS/pod-bank-rails.csr" \
    -subj "/CN=pod-bank-rails/O=fintech-sim-lab"
  openssl x509 -req \
    -in "$CERTS/pod-bank-rails.csr" \
    -CA "$CERTS/ca.pem" \
    -CAkey "$CERTS/ca-key.pem" \
    -CAcreateserial \
    -out "$CERTS/pod-bank-rails.pem" \
    -days "$DAYS" \
    -extfile /dev/stdin <<EOF
$POD_BR_SAN
extendedKeyUsage=serverAuth
EOF
  rm -f "$CERTS/pod-bank-rails.csr"
fi

# Mutual-TLS client cert: Banking Circle's mTLS listener requires every
# caller to present one (the e2e suite, standing in for "Infinite"), so this
# is no longer optional -- unconditional, same as the receiver/banking-circle
# server certs above.
if need "$CERTS/client.pem" || need "$CERTS/client-key.pem"; then
  echo "generating client cert"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$CERTS/client-key.pem" \
    -out "$CERTS/client.csr" \
    -subj "/CN=sim-client/O=fintech-sim-lab"
  openssl x509 -req \
    -in "$CERTS/client.csr" \
    -CA "$CERTS/ca.pem" \
    -CAkey "$CERTS/ca-key.pem" \
    -CAcreateserial \
    -out "$CERTS/client.pem" \
    -days "$DAYS" \
    -extfile /dev/stdin <<EOF
extendedKeyUsage=clientAuth
EOF
  rm -f "$CERTS/client.csr"
fi

chmod 644 "$CERTS/ca.pem" "$CERTS/receiver.pem" "$CERTS/banking-circle.pem" "$CERTS/pod-bank-rails.pem" 2>/dev/null || true
# 644, not 600: these are lab-only fake keys (gitignored, never real key
# material). The ca service writes them as one UID; receiver/notifier/
# banking-circle run as a different non-root UID (65532) in their own
# containers and must be able to read them. A real CA would never ship
# keys this way.
chmod 644 "$CERTS/ca-key.pem" "$CERTS/receiver-key.pem" "$CERTS/banking-circle-key.pem" "$CERTS/pod-bank-rails-key.pem" 2>/dev/null || true
[ -f "$CERTS/client-key.pem" ] && chmod 644 "$CERTS/client-key.pem" 2>/dev/null
true
echo "certs ready in $CERTS"
ls -l "$CERTS"
