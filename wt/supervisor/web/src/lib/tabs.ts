export const tabs = ["overview", "settings", "backups", "database", "deployments"] as const

export type Tab = (typeof tabs)[number]

export const tabLabels: { [_: string]: string } = {
	backups: "Backups",
	database: "Database",
	deployments: "Deployments",
	overview: "Overview",
	settings: "Settings",
}
