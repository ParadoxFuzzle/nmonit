/// Build script: generate Rust protobuf + gRPC code via tonic-prost-build.
///
/// This reads the same .proto files used by buf for Go codegen,
/// ensuring protocol consistency across Rust and Go components.
///
/// Note: Proto files are compiled in dependency order. Files that import
/// other proto files must be compiled after their dependencies.
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let proto_dir = concat!(env!("CARGO_MANIFEST_DIR"), "/../proto");
    let compute_dir = format!("{}/compute/v1", proto_dir);

    // Compile all proto files used by the agent.
    // resource.proto (no imports) → agent.proto → job.proto → memory.proto
    tonic_prost_build::configure()
        .build_server(false) // Agent is a gRPC client to the control plane
        .build_client(true)
        .compile_protos(
            &[
                // Base types — no imports
                &format!("{}/resource.proto", compute_dir),
                // Agent service — imports resource.proto
                &format!("{}/agent.proto", compute_dir),
                // Job service — imports resource.proto + agent.proto
                &format!("{}/job.proto", compute_dir),
                // Memory service — imports agent.proto
                &format!("{}/memory.proto", compute_dir),
                // Storage service — standalone
                &format!("{}/storage.proto", compute_dir),
                // Control service — standalone
                &format!("{}/control.proto", compute_dir),
            ],
            &[&proto_dir.to_string(), &compute_dir],
        )?;

    Ok(())
}
