/// Rust SDK for the distributed compute fabric.
///
/// Provides idiomatic Rust bindings for:
/// - Distributed memory allocation
/// - GPU memory allocation
/// - Job submission
/// - Cluster management
pub fn add(left: u64, right: u64) -> u64 {
    left + right
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn it_works() {
        let result = add(2, 2);
        assert_eq!(result, 4);
    }
}
