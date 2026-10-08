//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"syscall/js"
	"time"
)

func main() {
	state := &engine{}
	run := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) != 1 || args[0].Type() != js.TypeString {
			return js.Null()
		}
		input := args[0].String()
		executor := js.FuncOf(func(_ js.Value, callbacks []js.Value) any {
			resolve := callbacks[0]
			go func() {
				result := map[string]any{}
				var r request
				if len(input) > 2<<20 || json.Unmarshal([]byte(input), &r) != nil {
					result["error"] = "Invalid ENV operation."
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					data, err := state.run(ctx, r)
					if err != nil {
						result["error"] = err.Error()
					} else {
						result["data"] = data
					}
				}
				raw, _ := json.Marshal(result)
				resolve.Invoke(string(raw))
			}()
			return nil
		})
		promise := js.Global().Get("Promise").New(executor)
		executor.Release()
		return promise
	})
	js.Global().Set("paperboatEnvironmentVault", run)
	select {}
}
