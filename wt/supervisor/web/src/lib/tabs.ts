export const tabs = ["overview", "backups", "database"] as const

export type Tab = (typeof tabs)[number]

export const tabLabels: { [_: string]: string } = {
	backups: "Backups",
	database: "Database",
	overview: "Overview",
}
