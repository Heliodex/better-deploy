import { join } from "node:path"

export const root = join(import.meta.dir, "..")

export const localDir = join(root, ".local")
export const logsDir = join(localDir, "logs")
export const seaweedDataDir = join(localDir, "seaweed", "data")
export const seaweedConfig = join(root, "dev", "seaweed-s3.json")

export const pidFiles = {
	celld: join(localDir, "celld.pid"),
	weed: join(localDir, "weed.pid"),
}

const host = "127.0.0.1"

export const weedPorts = {
	filer: 8888,
	master: 9333,
	s3: 8333,
	volume: 8082,
}

export const s3 = {
	accessKeyId: "celld",
	bucket: "celld",
	endpoint: `http://${host}:${weedPorts.s3}`,
	region: "us-east-1",
	secretAccessKey: "celld-local-secret",
}

export const weed = {
	master: `${host}:${weedPorts.master}`,
}

export const celld = {
	internalListen: `${host}:8788`,
	listen: `${host}:8787`,
}

export const fleetBucket = `s3://${s3.bucket}`

// Local-only credentials; these are not secrets.
Object.assign(process.env, {
	AWS_ACCESS_KEY_ID: s3.accessKeyId,
	AWS_REGION: s3.region,
	AWS_SECRET_ACCESS_KEY: s3.secretAccessKey,
})
