package db

import "testing"

// Applied migration bytes are frozen, permanently and with no repair path.
//
// Open() records each migration's sha256 in schema_migrations and
// verifyApplied fails CLOSED when the embedded bytes no longer match, so
// editing an already-released migration refuses startup on every database
// that applied it. The checksum covers the WHOLE FILE — comments and
// whitespace included — which is what makes this trap so easy to walk
// into: a comment edit looks like the safest change there is.
//
// There is no way back. Production code only ever INSERTs into
// schema_migrations; nothing updates or deletes a recorded row outside
// tests, so a checksum is write-once per version per database. A new
// migration does not help either — migration 013 never touches version
// 11's row. The only remedy is not doing it.
//
// This table is the guard rail. TestMigrateRejectsEditedMigration proves
// the MECHANISM by tampering with a ledger row, but nothing pinned the
// embedded bytes themselves, so an edit sailed through a green suite:
// every test builds a fresh database that records the new checksum and
// then agrees with itself. Only a golden value disagrees.
//
// A failure here is not a broken test. It means a released migration was
// edited, and the fix is to restore its bytes exactly.
//
// Every embedded migration is pinned, and the test fails when one is not,
// so adding a migration means adding its checksum here in the same change.
var frozenMigrationChecksums = map[int]string{
	1:  "cb0602027d66ab73b1414569f3941a9d3eef3e546cca5b1d12a9c8872a9c9ad5",
	2:  "b94fe805508476bc761f048a3a1b168888a7e281da2be8b76ff0d165016cccf3",
	3:  "2878d69837a1fdbdb2344399f7cd364b93e825148c16c3e3cb5ea566204e988e",
	4:  "3e3062aaff412f9f571dbbdd020dd91845701645333c38702427454673620ef6",
	5:  "5b0e97f624d0e2e3b0e0fe779511c84c9884e582c393296db887754b1001f150",
	6:  "15ea3615109b23e0fc9899797cdf0a0b57901cd210f53f72cc58ea1693d64ded",
	7:  "feb64815ed15e87b59a6fe94445b47d73b90c27d0f44cb861588635fc7537cb0",
	8:  "0108c9751cf747c260b09ab636f65e0a2af92be45fc5415fc6676468bdec57bf",
	9:  "a36d2db8e0d4749a9c4f7b729fb7f1724cf181dee3a74d4dbeb610b5b975c940",
	10: "467561f16c7e64f53ca63b44fe803a7bb30fdd801681284928dcd987caadd78c",
	11: "0532a36e528c8f9860077bdcc6909bbb5031fa93f95f818ffe782cc928e053f5",
	12: "43ad357a21e3c9aeae1e7e6b73e464bbef83f2c6e246ed7c5f6de4a91138d4d7",
	13: "8da42ea5ef35a8568456dc8dc34e19e8807f21cd37728d6c2dd5f9fefdab9de7",
	14: "43587a7aa7a9be8fecaa82de024169932686509e9f1515e07967397fb7f94ddc",
	15: "195de8cd843b6ddfb1276ab4ec77f13cb5d3d40bf831842c5e53016f7c421ce1",
	16: "dbae9efdcd1b213bda9f6d094c4b0e7c7c98f04b2edf6be49c7094db7e804450",
	17: "beb94d69cb9aff8d3b2a99e1c06998f12514bad75bc2b6bb243ba55b8ab8b56b",
	18: "a755dde99f8a02591975e0c40933a0cf5846d7424fb6b05447306fb5870c23b7",
	19: "e414e2ab41a3258e9b1e7a89abf94a2a2a6543be39e3bc1ef1b2cac59a8d0cc6",
	20: "50be6c00bffa7fbae6bee1af42b082744e3480eee6ba0f2e5577630b5bc2feb9",
	21: "c3488a041123ba894a9d49bc05cfe0e0f5cb806b91203b858503d7770c3d7fdb",
	22: "616ab24c643643f18d8df64369fa08f5f95841012e56337ae1838d089d8678d1",
	23: "26aff9625055483410dcf3f98ed6ba96eceabfae1af3f7a352061ce82358f574",
	24: "535de6489a9b3c8ffa74e420c6983d107431024b4fd9ab7f2aee5e621fa39ffb",
	25: "0a60b1943f64dab9fd61726512b31f33546e2edc956b70c9d4677e853bd4f8c9",
	26: "29a36d2b4e58161f3642127175602b832489f534ff574699fd93d0598ecb150a",
	27: "4b15a8a38b8e3dc22556c21721b874f15dc84c47ab0775597b1776656b07b26e",
	28: "429de7d333075bdc8688603893d87f048fd48dac2f4f8a0f577a9f67fcf36ceb",
	29: "4ee2c501b7f2c05e2d32a8738883638ca3b6ca047890c10301c5c1adfc18d4a0",
	30: "d800076fee326d5e435dff5c9c8f4ee59c1aca89585d036a7798179707bbd244",
	31: "3702fde3e0a1de83f28e84bd0de54eb6f1d96d47133f30f2f7c4b2c4b8bfbe65",
	32: "844303cd9268cd6c2b0c557ff96f3fb051e94a6520f22fc66e35ba56cba81eb8",
	33: "ffc186fff4b43e9b9548fe033f9d612b3eebf3c9595ef936e4dc836ef9db070d",
	34: "a0bd365cf192fd5818097a4c9e20fb0b84ab19409d70178b35d1ea74df9c2c68",
	35: "c926c49b4f839150b7617ea57aa160c512d61568e903c8a709e747f7819ca342",
	36: "f73ba170c5dd1ea450a589a2e68490b0acbf953b0bcd956bf9c3c1752c64587e",
	37: "0e9fb32018c019bb45f9f9d10c709c8c0e84f73a238b54b1687acb8dd8dc7040",
	38: "101f8fb058f3fc35b32c58aea9da783413b3bd6672f5d4df37ccdaaceec1f306",
	39: "a388ca90925cae27806490684abf1e9861247e64bc514271c90bf57d38e501ea",
	40: "484044bfd1374489b32b728ac4d892d959356a6a7022cb3f446977418dbf267d",
}

func TestReleasedMigrationBytesAreFrozen(t *testing.T) {
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	embedded := make(map[int]Migration, len(migrations))
	for _, m := range migrations {
		embedded[m.Version] = m
	}

	for version, want := range frozenMigrationChecksums {
		m, ok := embedded[version]
		if !ok {
			t.Errorf("migration %d is frozen but no longer embedded — a released migration was deleted", version)
			continue
		}
		if m.Checksum != want {
			t.Errorf(`migration %d (%q) was edited after release.

Its bytes are recorded in every database that already applied it, and
verifyApplied refuses startup when they disagree. There is no repair
path: the recorded checksum is write-once, and a new migration does not
replace it. Restore the file exactly.

  embedded %s
  frozen   %s

Only if this migration has never been released may the frozen value be
updated instead.`, version, m.Name, m.Checksum, want)
		}
	}

	// Every embedded migration must be pinned, or the pin list falls behind
	// the moment one is added and the loop above silently skips it. That is
	// also the one way a green suite could still hide an edit: delete a
	// golden value and the migration stops being checked at all.
	for _, m := range migrations {
		if _, ok := frozenMigrationChecksums[m.Version]; !ok {
			t.Errorf(`migration %d (%q) has no frozen checksum.

Every embedded migration is pinned here, so that editing one cannot pass
CI. Add the line below to frozenMigrationChecksums in the same change
that adds the migration:

	%d: %q,`, m.Version, m.Name, m.Version, m.Checksum)
		}
	}
}
