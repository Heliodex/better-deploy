import { pidFiles } from "./config"
import { stopBackground } from "./processes"

const stopped: string[] = []

// Stop the Worker node first, then the storage backing it.
if (await stopBackground(pidFiles.celld, "celld")) stopped.push("celld")
if (await stopBackground(pidFiles.weed, "weed")) stopped.push("weed")

if (stopped.length === 0) console.log("nothing running")
