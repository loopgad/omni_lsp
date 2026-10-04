from base import Base, convert


class Child(Base):
    def ping(self, value: int) -> str:
        return convert(value)


def run() -> str:
    child = Child()
    return child.ping(7)
