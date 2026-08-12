ALTER TABLE campaigns ADD COLUMN header_image_url TEXT;
ALTER TABLE campaigns ADD COLUMN template_params_json TEXT NOT NULL DEFAULT '{}';
