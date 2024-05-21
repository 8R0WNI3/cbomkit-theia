package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/assert"
)

var testfileFolder string = "./testdata"
var filesystemExtension string = "/filesystem"
var outputExtension string = "/out/bom.json"
var bomFolderExtension string = "/in/bom.json"

var tests = []struct {
	in  string
	err bool
}{
	{"/1", false},
}

func TestScan(t *testing.T) {
	for _, test := range tests {
		t.Run(test.in+", BOM: "+test.in, func(t *testing.T) {
			bomFolder := testfileFolder + test.in + bomFolderExtension
			filesystemPath := testfileFolder + test.in + filesystemExtension

			tempTarget, err := os.CreateTemp("", "TestParseBOM")
			if err != nil {
				panic(err)
			}

			err = run(&bomFolder, &filesystemPath, tempTarget)
			if test.err {
				assert.Error(t, err, "scan did not fail although it should")
			} else {
				assert.NoError(t, err, "scan did fail although it should not")
			}
			
			output, err := os.ReadFile(tempTarget.Name())
			if err != nil {
				panic(err)
			}

			trueOutput, err := os.ReadFile(testfileFolder + test.in + outputExtension)
			if err != nil {
				panic(err)
			}

			require.JSONEq(t, string(output), string(trueOutput), "resulting JSONs do not equal")
			os.Remove(tempTarget.Name())
		})
	}
}
