fn greet(name: &str) -> String {
    format!("hello, {}", name)
}

fn main() {
    let msg = greet("world");
    println!("{}", msg);
}
