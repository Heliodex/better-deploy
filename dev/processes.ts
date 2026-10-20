import { closeSync, openSync } from "node:fs"
import { mkdir, rm } from "node:fs/promises"
import { dirname } from "node:path"

import { root } from "./config"

export async function readPid(pidFile: string): Promise<number | undefined> {
	const file = Bun.file(pidFile)
	if (!(await file.exists())) return undefined

	const pid = Number.parseInt((await file.text()).trim(), 10)
	return Number.isInteger(pid) && pid > 0 ? pid : undefined
}

export function isAlive(pid: number): boolean {
	try {
		process.kill(pid, 0)
		return true
	} catch {
		return false
	}
}

export async function isRunning(pidFile: string, name: string): Promise<boolean> {
	const pid = await readPid(pidFile)
	if (pid === undefined || !isAlive(pid)) return false

	// Guard against a recycled pid before we trust or kill it.
	if (process.platform === "linux") {
		try {
			const cmdline = await Bun.file(`/proc/${pid}/cmdline`).text()
			return cmdline.includes(name)
		} catch {
			return false
		}
	}
	return true
}

export async function startBackground(command: string[], pidFile: string, logFile: string): Promise<number> {
	await mkdir(dirname(pidFile), { recursive: true })
	await mkdir(dirname(logFile), { recursive: true })

	const log = openSync(logFile, "w")
	const proc = Bun.spawn(command, {
		cwd: root,
		env: { ...process.env },
		stderr: log,
		stdin: "ignore",
		stdout: log,
	})
	closeSync(log)
	proc.unref()

	await Bun.write(pidFile, `${proc.pid}\n`)
	return proc.pid
}

export async function stopBackground(pidFile: string, name: string, graceMs = 30_000): Promise<boolean> {
	const pid = await readPid(pidFile)
	if (pid === undefined || !(await isRunning(pidFile, name))) {
		await rm(pidFile, { force: true })
		return false
	}

	console.log(`stopping ${name} (pid ${pid})...`)
	try {
		process.kill(pid, "SIGTERM")
	} catch {}

	// SeaweedFS can take several seconds to flush and close its stores.
	const deadline = Date.now() + graceMs
	while (Date.now() < deadline && isAlive(pid)) await Bun.sleep(250)

	if (isAlive(pid)) {
		try {
			process.kill(pid, "SIGKILL")
		} catch {}
		const killDeadline = Date.now() + 5_000
		while (Date.now() < killDeadline && isAlive(pid)) await Bun.sleep(100)
	}

	await rm(pidFile, { force: true })
	return true
}
