package reed

import stdbase64 "encoding/base64"

func base64(b []byte) string { return stdbase64.StdEncoding.EncodeToString(b) }
