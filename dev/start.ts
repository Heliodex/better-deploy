import { mkdir } from "node:fs/promises"
import { join } from "node:path"
import { $ } from "bun"

import {
	celld,
	fleetBucket,
	logsDir,
	pidFiles,
	root,
	s3,
	seaweedConfig,
	seaweedDataDir,
	weed,
	weedPorts,
} from "./config"
import { isRunning, readPid, startBackground } from "./processes"

const appUrl = `http://${celld.listen}`

await mkdir(seaweedDataDir, { recursive: true })
await mkdir(logsDir, { recursive: true })

async function fail(message: string, logFile: string): Promise<never> {
	console.error(message)
	const tail = await $`tail -n 20 ${logFile}`.nothrow().quiet()
	const output = tail.stdout.toString().trim()
	if (output) console.error(output)
	process.exit(1)
}

function weedShell(script: string) {
	return $`weed shell -master=${weed.master} < ${new Response(script)}`.nothrow()
}

// --- SeaweedFS -----------------------------------------------------------------

const weedLog = join(logsDir, "seaweed.log")

if (await isRunning(pidFiles.weed, "weed")) {
	console.log(`SeaweedFS already running (pid ${await readPid(pidFiles.weed)})`)
} else {
	const pid = await startBackground(
		[
			"weed",
			"server",
			`-dir=${seaweedDataDir}`,
			"-ip=127.0.0.1",
			"-ip.bind=127.0.0.1",
			`-master.port=${weedPorts.master}`,
			`-volume.port=${weedPorts.volume}`,
			`-filer.port=${weedPorts.filer}`,
			"-s3",
			`-s3.port=${weedPorts.s3}`,
			`-s3.config=${seaweedConfig}`,
		],
		pidFiles.weed,
		weedLog,
	)
	console.log(`starting SeaweedFS (S3 on ${s3.endpoint}, pid ${pid})...`)
}

process.stdout.write("waiting for S3 endpoint ")
const s3Deadline = Date.now() + 60_000
let s3Up = false
while (Date.now() < s3Deadline) {
	try {
		// SeaweedFS answers unauthenticated requests with 403, which is fine.
		const response = await fetch(`${s3.endpoint}/`, { signal: AbortSignal.timeout(1_000) })
		if (response.status === 200 || response.status === 403) {
			s3Up = true
			break
		}
	} catch {}
	process.stdout.write(".")
	await Bun.sleep(500)
}
console.log(s3Up ? " ready" : " timed out")
if (!s3Up) await fail(`SeaweedFS S3 did not come up on ${s3.endpoint}`, weedLog)

// Buckets are metadata in the filer; create the fleet bucket if it is missing.
const bucketList = await weedShell("s3.bucket.list\n").quiet()
const bucketExists = new RegExp(`^\\s*${s3.bucket}\\s`, "m").test(bucketList.stdout.toString())

if (!bucketExists) {
	console.log(`creating bucket ${s3.bucket}`)
	const created = await weedShell(`s3.bucket.create -name ${s3.bucket}\n`)
	if (created.exitCode !== 0) await fail(`Could not create bucket ${s3.bucket}`, weedLog)
}

console.log("checking the bucket:")
// Bind probes use port 0 so this also works while celld is already running.
const diagnosis =
	await $`celld diagnose --bucket ${fleetBucket} --endpoint ${s3.endpoint} --listen 127.0.0.1:0 --internal-listen 127.0.0.1:0`.nothrow()
if (diagnosis.exitCode !== 0) process.exit(diagnosis.exitCode)

// --- celld ---------------------------------------------------------------------

// Deploy before starting the node so the first request is served immediately.
await $`celld deploy ${root} --bucket ${fleetBucket} --endpoint ${s3.endpoint}`

const celldLog = join(logsDir, "celld.log")

if (await isRunning(pidFiles.celld, "celld")) {
	console.log(`celld already running (pid ${await readPid(pidFiles.celld)}); it adopts the new deployment`)
} else {
	const pid = await startBackground(
		[
			"celld",
			"--bucket",
			fleetBucket,
			"--endpoint",
			s3.endpoint,
			"--listen",
			celld.listen,
			"--internal-listen",
			celld.internalListen,
		],
		pidFiles.celld,
		celldLog,
	)
	console.log(`starting celld on ${celld.listen} (pid ${pid})...`)
}

process.stdout.write("waiting for the app ")
const appDeadline = Date.now() + 120_000
let body: string | undefined
while (Date.now() < appDeadline) {
	try {
		const response = await fetch(`${appUrl}/?name=__startup`, { signal: AbortSignal.timeout(5_000) })
		if (response.ok) {
			body = await response.text()
			break
		}
	} catch {}
	process.stdout.write(".")
	await Bun.sleep(1_000)
}
console.log(body === undefined ? " timed out" : " ready")
if (body === undefined) await fail(`celld did not serve the app on ${appUrl}`, celldLog)

console.log()
console.log(body)
console.log()
console.log(`App:        ${appUrl}/?name=dev`)
console.log(`S3 bucket:  ${fleetBucket} at ${s3.endpoint}`)
console.log(`Logs:       ${logsDir}/celld.log, ${logsDir}/seaweed.log`)
