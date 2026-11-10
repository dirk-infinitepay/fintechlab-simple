package b4b

import "github.com/webduvet/fintechlab-simple/internal/tenant"

// Environments (docs/lab-api/environments.md): every platform using the lab
// is one B4B customer, known by the key its JWTs are signed with, and each
// record here belongs to the environment that made it. A company carries its
// owner, and so does everything keyed by it (addresses, people, the extended
// profile, vIBANs); an upload, a beneficiary and a payment carry their own.
//
// The stores are read through a view of one environment (Directory.In,
// BeneficiaryStore.In): it lists, finds and changes only that environment's
// records, so another environment's id reads as an unknown one, and an
// external_ref is unique within an environment, not across them. The view
// NewDirectory and NewBeneficiaryStore return is the default environment's,
// which is the whole store as it was before there were environments.
//
// The owner is kept in the store and in the state file, never on the wire:
// the copies a view hands out leave it empty, because the real vendor's
// shapes have no such field and the caller named the environment to get
// them.

// owner is how a record stores the environment that owns it: empty for the
// default, so a state file from before environments reads as the default's
// and the default's records are written exactly as they always were.
func owner(env string) string {
	if env = tenant.Normalize(env); env == tenant.Default {
		return ""
	}
	return env
}

// refKey indexes an external_ref within its owner's records.
func refKey(own, ref string) string { return own + "\x00" + ref }
