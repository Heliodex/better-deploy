import { $ } from "bun"

import { fleetBucket, root, s3 } from "./config"

// A running celld node polls the deployment pointer and adopts this version.
await $`celld deploy ${root} --bucket ${fleetBucket} --endpoint ${s3.endpoint}`
