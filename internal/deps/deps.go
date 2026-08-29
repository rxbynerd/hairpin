// Package deps pins module dependencies that scaffolded packages do not
// import yet, so go.mod/go.sum stay stable while implementation lands
// in parallel. Delete once every pinned module has a real importer.
package deps

import (
	_ "connectrpc.com/connect"
	_ "github.com/alicebob/miniredis/v2"
	_ "github.com/redis/go-redis/v9"
	_ "golang.org/x/net/http2"
	_ "golang.org/x/net/http2/h2c"
	_ "google.golang.org/protobuf/encoding/protojson"
	_ "k8s.io/api/batch/v1"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/tools/clientcmd"
)
