package aiproxy

import "github.com/pai801/myapi/relay/adaptor/openai"

var ModelList = []string{""}

func init() {
	ModelList = openai.ModelList
}
