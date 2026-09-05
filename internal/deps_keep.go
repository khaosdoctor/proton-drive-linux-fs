package internal

// ponytail: keeps deps pinned until real code imports them; delete when unused
import (
	_ "github.com/ProtonMail/gopenpgp/v2/crypto"
	_ "github.com/hanwen/go-fuse/v2/fs"
	_ "github.com/henrybear327/go-proton-api"
)
