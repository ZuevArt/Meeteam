export type Event = {
  id: string;
  source_id: string;
  source_name: string;
  source_event_id: string;
  title: string;
  description?: string;
  start_at?: string;
  end_at?: string;
  start_precision: "date" | "datetime";
  registration_deadline_at?: string;
  listing_expires_at?: string;
  format?: "online" | "offline" | "hybrid" | string;
  organizer?: string;
  official_url: string;
  source_url: string;
  image_url?: string;
  tags?: string[];
  parsing_notes?: string[];
  last_seen_at: string;
};

export type Source = {
  id: string;
  name: string;
  base_url: string;
  import_order: number;
  enabled: boolean;
  status: "never" | "running" | "success" | "partial" | "failed";
  adapter_version: string;
  last_attempt_at?: string;
  last_success_at?: string;
  last_error?: string;
  fetched_count: number;
  valid_count: number;
  rejected_count: number;
};

export type ImportRun = {
  id: string;
  status: string;
  fetched_count: number;
  valid_count: number;
  rejected_count: number;
  error_summary?: string;
  finished_at: string;
};
