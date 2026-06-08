fn main() -> Result<(), Box<dyn std::error::Error>> {
    if std::env::var_os("PROTOC").is_none() {
        std::env::set_var("PROTOC", protoc_bin_vendored::protoc_bin_path()?);
    }
    let protos = [
        "../proto/codeintel/v1/index_plan_service.proto",
        "../proto/codeintel/v1/index_executor_service.proto",
    ];
    tonic_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_protos(&protos, &["../proto"])?;
    for proto in protos {
        println!("cargo:rerun-if-changed={proto}");
    }
    Ok(())
}
