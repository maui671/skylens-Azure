fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Proto lives at repo root (shared between tap and node)
    prost_build::compile_protos(&["../proto/skylens.proto"], &["../proto/"])?;
    Ok(())
}
