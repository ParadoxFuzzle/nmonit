/// Build script: generate Rust protobuf + gRPC code via tonic-build.
///
/// This reads the same .proto files used by buf for Go codegen,
/// ensuring protocol consistency across Rust and Go components.
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let proto_dir = concat!(env!("CARGO_MANIFEST_DIR"), "/../proto");

    tonic_build::configure()
        .build_server(false) // Agent is a gRPC client to the control plane
        .build_client(true)
        .compile_protos(
            &[
                &format!("{}/compute/v1/agent.proto", proto_dir),
                &format!("{}/compute/v1/resource.proto", proto_dir),
                &format!("{}/compute/v1/job.proto", proto_dir),
            ],
            &[proto_dir],
        )?;

    Ok(())
}
