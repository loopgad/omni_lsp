def describe(count: int) -> str:
    return f"items: {count}"


def main() -> None:
    summary = describe(3)
    print(summary)
