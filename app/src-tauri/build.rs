use std::path::Path;

// Builds the Tauri context. With the default custom-protocol feature the context bakes ../dist in, so a fresh clone that has not run `pnpm build` yet gets a plain message here instead of generate_context!'s error about a missing directory that never names pnpm.
fn main() {
    if std::env::var_os("CARGO_FEATURE_CUSTOM_PROTOCOL").is_some() && !Path::new("../dist").is_dir()
    {
        panic!("app/dist is missing: run `pnpm build` in app/ first (or build with --no-default-features to load the Vite dev server instead)");
    }
    tauri_build::build()
}
