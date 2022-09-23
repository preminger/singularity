// Copyright (c) 2018-2021, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package main

// Note that the inclusion of builddir here only works when mconfig -b has not
//  renamed it; that is handled via a setting of CGO_CFLAGS in mconfig. It is
//  included here also so that Go tools such as code editors and linters can
//  find config.h when the default builddir is used.

// #cgo CFLAGS: -I${SRCDIR}/../../builddir
// #include <config.h>
// #include "c/message.c"
// #include "c/capability.c"
// #include "c/setns.c"
// #include "c/starter.c"
import "C"

import (
	"runtime"
	"unsafe"

	"github.com/sylabs/singularity/internal/app/starter"
	"github.com/sylabs/singularity/internal/pkg/runtime/engine"
	starterConfig "github.com/sylabs/singularity/internal/pkg/runtime/engine/config/starter"
	"github.com/sylabs/singularity/internal/pkg/util/mainthread"
	"github.com/sylabs/singularity/pkg/sylog"

	// register engines
	_ "github.com/sylabs/singularity/cmd/starter/engines"
)

func getEngine(jsonConfig []byte) *engine.Engine {
	e, err := engine.Get(jsonConfig)
	if err != nil {
		sylog.Fatalf("Failed to initialize runtime engine: %s\n", err)
	}
	return e
}

func startup() {
	// global variable defined in cmd/starter/c/starter.c,
	// C.sconfig points to a shared memory area
	csconf := unsafe.Pointer(C.sconfig)
	// initialize starter configuration
	sconfig := starterConfig.NewConfig(starterConfig.SConfig(csconf))
	// get JSON configuration originally passed from CLI
	jsonConfig := sconfig.GetJSONConfig()

	// get engine operations previously registered
	// by the above import
	e := getEngine(jsonConfig)
	sylog.Debugf("%s runtime engine selected", e.EngineName)

	switch C.goexecute {
	case C.STAGE1:
		sylog.Verbosef("Execute stage 1\n")
		starter.StageOne(sconfig, e)
	case C.STAGE2:
		sylog.Verbosef("Execute stage 2\n")
		if err := sconfig.Release(); err != nil {
			sylog.Fatalf("%s", err)
		}

		mainthread.Execute(func() {
			starter.StageTwo(int(C.master_socket[1]), e)
		})
	case C.MASTER:
		sylog.Verbosef("Execute master process\n")

		pid := sconfig.GetContainerPid()
		imageFd := sconfig.GetImageFd()

		if err := sconfig.Release(); err != nil {
			sylog.Fatalf("%s", err)
		}

		starter.Master(int(C.rpc_socket[0]), int(C.master_socket[0]), int(C.cleanup_socket[0]), pid, imageFd, e)
	case C.RPC_SERVER:
		sylog.Verbosef("Serve RPC requests\n")

		if err := sconfig.Release(); err != nil {
			sylog.Fatalf("%s", err)
		}

		starter.RPCServer(int(C.rpc_socket[1]), e)
	case C.CLEANUP_HOST:
		sylog.Verbosef("Execute Cleanup Host Process")
		if err := sconfig.Release(); err != nil {
			sylog.Fatalf("%s", err)
		}
		starter.CleanupHost(int(C.cleanup_socket[1]), e)
	}

	sylog.Fatalf("You should not be there\n")
}

func init() {
	// lock main thread for function execution loop
	runtime.LockOSThread()
	// this is mainly to reduce memory footprint
	runtime.GOMAXPROCS(1)
}

// main function is executed after starter.c init function.
// Depending on the value of goexecute from starter.c Go will act differently,
// e.g. it may launch container process or spawn a container monitor. Thus
// Go runtime appears to be in a different environment based on the current
// execution stage.
func main() {
	// spawn a goroutine to use mainthread later
	go startup()

	// run functions requiring execution in main thread
	for f := range mainthread.FuncChannel {
		f()
	}
}
