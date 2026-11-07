package httputilx

import "crypto/x509"

// RootsWithLabCA is the trust store for the lab's outbound HTTPS: the
// system's roots plus the lab CA in pem.
//
// The lab CA alone was enough while every webhook target was a lab
// container (the receiver) or a platform on the same host with a lab
// certificate. A lab deployed somewhere also calls platforms behind public
// certificates -- a webhook endpoint on a load balancer -- and those must
// verify too, so the lab CA is added to the system roots, not put in their
// place. ok is false when pem holds no certificate.
func RootsWithLabCA(pem []byte) (pool *x509.CertPool, ok bool) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	return pool, pool.AppendCertsFromPEM(pem)
}
