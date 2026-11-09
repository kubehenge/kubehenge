### Changed

- The add-on room check counts the memory the add-ons' pods request, as well as what they use.
- A worker that holds what the add-ons use but not what they request is refused, as their pods would stay pending.
